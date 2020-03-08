// Copyright (c) 2012-2014 Jeremy Latt
// Copyright (c) 2016-2017 Daniel Oaks <daniel@danieloaks.net>
// released under the MIT license

package irc

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/zszwede/mod_irc_server/irc/utils"
)

var (
	handshakeTimeout, _ = time.ParseDuration("5s")
	errSendQExceeded    = errors.New("SendQ exceeded")

	sendQExceededMessage = []byte("\r\nERROR :SendQ Exceeded\r\n")
)

// Socket represents an IRC socket.
type Socket struct {
	sync.Mutex

	conn   net.Conn
	reader *bufio.Reader

	maxSendQBytes int

	// this is a trylock enforcing that only one goroutine can write to `conn` at a time
	writerSemaphore utils.Semaphore

	buffers       [][]byte
	totalLength   int
	closed        bool
	sendQExceeded bool
	finalData     []byte // what to send when we die
	finalized     bool
}

// NewSocket returns a new Socket.
func NewSocket(conn net.Conn, maxReadQBytes int, maxSendQBytes int) *Socket {
	result := Socket{
		conn:          conn,
		reader:        bufio.NewReaderSize(conn, maxReadQBytes),
		maxSendQBytes: maxSendQBytes,
	}
	result.writerSemaphore.Initialize(1)
	return &result
}

// Close stops a Socket from being able to send/receive any more data.
func (socket *Socket) Close() {
	socket.Lock()
	socket.closed = true
	socket.Unlock()

	socket.wakeWriter()
}

// CertFP returns the fingerprint of the certificate provided by the client.
func (socket *Socket) CertFP() (string, error) {
	var tlsConn, isTLS = socket.conn.(*tls.Conn)
	if !isTLS {
		return "", errNotTLS
	}

	// ensure handehake is performed, and timeout after a few seconds
	tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
	err := tlsConn.Handshake()
	tlsConn.SetDeadline(time.Time{})

	if err != nil {
		return "", err
	}

	peerCerts := tlsConn.ConnectionState().PeerCertificates
	if len(peerCerts) < 1 {
		return "", errNoPeerCerts
	}

	rawCert := sha256.Sum256(peerCerts[0].Raw)
	fingerprint := hex.EncodeToString(rawCert[:])

	return fingerprint, nil
}

// Read returns a single IRC line from a Socket.
func (socket *Socket) Read() (string, error) {
	if socket.IsClosed() {
		return "", io.EOF
	}

	lineBytes, isPrefix, err := socket.reader.ReadLine()
	if isPrefix {
		return "", errReadQ
	}

	// convert bytes to string
	line := string(lineBytes)

	// read last message properly (such as ERROR/QUIT/etc), just fail next reads/writes
	if err == io.EOF {
		socket.Close()
	}

	if err == io.EOF && strings.TrimSpace(line) != "" {
		// don't do anything
	} else if err != nil {
		return "", err
	}

	return line, nil
}

// Write sends the given string out of Socket. Requirements:
// 1. MUST NOT block for macroscopic amounts of time
// 2. MUST NOT reorder messages
// 3. MUST provide mutual exclusion for socket.conn.Write
// 4. SHOULD NOT tie up additional goroutines, beyond the one blocked on socket.conn.Write
func (socket *Socket) Write(data []byte) (err error) {
	if len(data) == 0 {
		return
	}

	socket.Lock()
	if socket.closed {
		err = io.EOF
	} else {
		prospectiveLen := socket.totalLength + len(data)
		if prospectiveLen > socket.maxSendQBytes {
			socket.sendQExceeded = true
			socket.closed = true
			err = errSendQExceeded
		} else {
			socket.buffers = append(socket.buffers, data)
			socket.totalLength = prospectiveLen
		}
	}
	socket.Unlock()

	socket.wakeWriter()
	return
}

// BlockingWrite sends the given string out of Socket. Requirements:
// 1. MUST block until the message is sent
// 2. MUST bypass sendq (calls to BlockingWrite cannot, on their own, cause a sendq overflow)
// 3. MUST provide mutual exclusion for socket.conn.Write
// 4. MUST respect the same ordering guarantees as Write (i.e., if a call to Write that sends
//    message m1 happens-before a call to BlockingWrite that sends message m2,
//    m1 must be sent on the wire before m2
// Callers MUST be writing to the client's socket from the client's own goroutine;
// other callers must use the nonblocking Write call instead. Otherwise, a client
// with a slow/unreliable connection risks stalling the progress of the system as a whole.
func (socket *Socket) BlockingWrite(data []byte) (err error) {
	if len(data) == 0 {
		return
	}

	// after releasing the semaphore, we must check for fresh data, same as `send`
	defer func() {
		if socket.readyToWrite() {
			socket.wakeWriter()
		}
	}()

	// blocking acquire of the trylock
	socket.writerSemaphore.Acquire()
	defer socket.writerSemaphore.Release()

	// first, flush any buffered data, to preserve the ordering guarantees
	closed := socket.performWrite()
	if closed {
		return io.EOF
	}

	_, err = socket.conn.Write(data)
	if err != nil {
		socket.finalize()
	}
	return
}

// wakeWriter starts the goroutine that actually performs the write, without blocking
func (socket *Socket) wakeWriter() {
	if socket.writerSemaphore.TryAcquire() {
		// acquired the trylock; send() will release it
		go socket.send()
	}
	// else: do nothing, the holder will check for more data after releasing it
}

// SetFinalData sets the final data to send when the SocketWriter closes.
func (socket *Socket) SetFinalData(data []byte) {
	socket.Lock()
	defer socket.Unlock()
	socket.finalData = data
}

// IsClosed returns whether the socket is closed.
func (socket *Socket) IsClosed() bool {
	socket.Lock()
	defer socket.Unlock()
	return socket.closed
}

// is there data to write?
func (socket *Socket) readyToWrite() bool {
	socket.Lock()
	defer socket.Unlock()
	// on the first time observing socket.closed, we still have to write socket.finalData
	return !socket.finalized && (socket.totalLength > 0 || socket.closed)
}

// send actually writes messages to socket.Conn; it may block
func (socket *Socket) send() {
	for {
		// we are holding the trylock: actually do the write
		socket.performWrite()
		// surrender the trylock, avoiding a race where a write comes in after we've
		// checked readyToWrite() and it returned false, but while we still hold the trylock:
		socket.writerSemaphore.Release()
		// check if more data came in while we held the trylock:
		if !socket.readyToWrite() {
			return
		}
		if !socket.writerSemaphore.TryAcquire() {
			// failed to acquire; exit and wait for the holder to observe readyToWrite()
			// after releasing it
			return
		}
		// got the lock again, loop back around and write
	}
}

// write the contents of the buffer, then see if we need to close
// returns whether we closed
func (socket *Socket) performWrite() (closed bool) {
	// retrieve the buffered data, clear the buffer
	socket.Lock()
	buffers := socket.buffers
	socket.buffers = nil
	socket.totalLength = 0
	closed = socket.closed
	socket.Unlock()

	var err error
	if 0 < len(buffers) {
		// on Linux, the runtime will optimize this into a single writev(2) call:
		_, err = (*net.Buffers)(&buffers).WriteTo(socket.conn)
	}

	closed = closed || err != nil
	if closed {
		socket.finalize()
	}
	return
}

// mark closed and send final data. you must be holding the semaphore to call this:
func (socket *Socket) finalize() {
	// mark the socket closed (if someone hasn't already), then write error lines
	socket.Lock()
	socket.closed = true
	finalized := socket.finalized
	socket.finalized = true
	finalData := socket.finalData
	if socket.sendQExceeded {
		finalData = sendQExceededMessage
	}
	socket.Unlock()

	if finalized {
		return
	}

	if len(finalData) != 0 {
		socket.conn.Write(finalData)
	}

	// close the connection
	socket.conn.Close()
}
