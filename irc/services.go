// Copyright (c) 2018 Shivaram Lingamneni <slingamn@cs.stanford.edu>
// released under the MIT license

package irc

import (
	"bytes"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/goshuirc/irc-go/ircfmt"
	"github.com/goshuirc/irc-go/ircmsg"
	"github.com/zszwede/mod_irc_server/irc/utils"
)

// defines an IRC service, e.g., NICKSERV
type ircService struct {
	Name           string
	ShortName      string
	prefix         string
	CommandAliases []string
	Commands       map[string]*serviceCommand
	HelpBanner     string
}

// defines a command associated with a service, e.g., NICKSERV IDENTIFY
type serviceCommand struct {
	aliasOf           string   // marks this command as an alias of another
	capabs            []string // oper capabs the given user has to have to access this command
	handler           func(server *Server, client *Client, command string, params []string, rb *ResponseBuffer)
	help              string
	helpStrings       []string
	helpShort         string
	enabled           func(*Config) bool // is this command enabled in the server config?
	authRequired      bool
	hidden            bool
	minParams         int
	maxParams         int  // optional, if set it's an error if the user passes more than this many params
	unsplitFinalParam bool // split into at most maxParams, with last param containing unsplit text
}

// looks up a command in the table of command definitions for a service, resolving aliases
func lookupServiceCommand(commands map[string]*serviceCommand, command string) *serviceCommand {
	maxDepth := 1
	depth := 0
	for depth <= maxDepth {
		result, ok := commands[command]
		if !ok {
			return nil
		} else if result.aliasOf == "" {
			return result
		} else {
			command = result.aliasOf
			depth += 1
		}
	}
	return nil
}

// all services, by lowercase name
var OragonoServices = map[string]*ircService{
	"nickserv": {
		Name:           "NickServ",
		ShortName:      "NS",
		CommandAliases: []string{"NICKSERV", "NS"},
		Commands:       nickservCommands,
		HelpBanner:     nickservHelp,
	},
	"chanserv": {
		Name:           "ChanServ",
		ShortName:      "CS",
		CommandAliases: []string{"CHANSERV", "CS"},
		Commands:       chanservCommands,
		HelpBanner:     chanservHelp,
	},
	"hostserv": {
		Name:           "HostServ",
		ShortName:      "HS",
		CommandAliases: []string{"HOSTSERV", "HS"},
		Commands:       hostservCommands,
		HelpBanner:     hostservHelp,
	},
}

// all service commands at the protocol level, by uppercase command name
// e.g., NICKSERV, NS
var oragonoServicesByCommandAlias map[string]*ircService

// special-cased command shared by all services
var servHelpCmd serviceCommand = serviceCommand{
	help: `Syntax: $bHELP [command]$b

HELP returns information on the given command.`,
	helpShort: `$bHELP$b shows in-depth information about commands.`,
}

// generic handler for IRC commands like `/NICKSERV INFO`
func serviceCmdHandler(server *Server, client *Client, msg ircmsg.IrcMessage, rb *ResponseBuffer) bool {
	service, ok := oragonoServicesByCommandAlias[msg.Command]
	if !ok {
		server.logger.Warning("internal", "can't handle unrecognized service", msg.Command)
		return false
	}

	if len(msg.Params) == 0 {
		return false
	}
	commandName := strings.ToLower(msg.Params[0])
	params := msg.Params[1:]
	cmd := lookupServiceCommand(service.Commands, commandName)
	// for a maxParams command, join all final parameters together if necessary
	if cmd != nil && cmd.unsplitFinalParam && cmd.maxParams < len(params) {
		newParams := make([]string, cmd.maxParams)
		copy(newParams, params[:cmd.maxParams-1])
		newParams[cmd.maxParams-1] = strings.Join(params[cmd.maxParams-1:], " ")
		params = newParams
	}
	serviceRunCommand(service, server, client, cmd, commandName, params, rb)
	return false
}

// generic handler for service PRIVMSG, like `/msg NickServ INFO`
func servicePrivmsgHandler(service *ircService, server *Server, client *Client, message string, rb *ResponseBuffer) {
	params := strings.Fields(message)
	if len(params) == 0 {
		return
	}

	// look up the service command to see how to parse it
	commandName := strings.ToLower(params[0])
	cmd := lookupServiceCommand(service.Commands, commandName)
	// reparse if needed
	if cmd != nil && cmd.unsplitFinalParam {
		params = utils.FieldsN(message, cmd.maxParams+1)[1:]
	} else {
		params = params[1:]
	}
	serviceRunCommand(service, server, client, cmd, commandName, params, rb)
}

// actually execute a service command
func serviceRunCommand(service *ircService, server *Server, client *Client, cmd *serviceCommand, commandName string, params []string, rb *ResponseBuffer) {
	nick := rb.target.Nick()
	sendNotice := func(notice string) {
		rb.Add(nil, service.prefix, "NOTICE", nick, notice)
	}

	if cmd == nil {
		sendNotice(fmt.Sprintf(client.t("Unknown command. To see available commands, run: /%s HELP"), service.ShortName))
		return
	}

	if len(params) < cmd.minParams || (0 < cmd.maxParams && cmd.maxParams < len(params)) {
		sendNotice(fmt.Sprintf(client.t("Invalid parameters. For usage, do /msg %[1]s HELP %[2]s"), service.Name, strings.ToUpper(commandName)))
		return
	}

	if cmd.enabled != nil && !cmd.enabled(server.Config()) {
		sendNotice(client.t("This command has been disabled by the server administrators"))
		return
	}

	if 0 < len(cmd.capabs) && !client.HasRoleCapabs(cmd.capabs...) {
		sendNotice(client.t("Command restricted"))
		return
	}

	if cmd.authRequired && client.Account() == "" {
		sendNotice(client.t("You're not logged into an account"))
		return
	}

	server.logger.Debug("services", fmt.Sprintf("Client %s ran %s command %s", client.Nick(), service.Name, commandName))
	if commandName == "help" {
		serviceHelpHandler(service, server, client, params, rb)
	} else {
		cmd.handler(server, client, commandName, params, rb)
	}
}

// generic handler that displays help for service commands
func serviceHelpHandler(service *ircService, server *Server, client *Client, params []string, rb *ResponseBuffer) {
	nick := rb.target.Nick()
	config := server.Config()
	sendNotice := func(notice string) {
		rb.Add(nil, service.prefix, "NOTICE", nick, notice)
	}

	sendNotice(ircfmt.Unescape(fmt.Sprintf("*** $b%s HELP$b ***", service.Name)))

	if len(params) == 0 {
		helpBannerLines := strings.Split(client.t(service.HelpBanner), "\n")
		helpBannerLines = append(helpBannerLines, []string{
			"",
			client.t("To see in-depth help for a specific command, try:"),
			ircfmt.Unescape(fmt.Sprintf(client.t("    $b/msg %s HELP <command>$b"), service.Name)),
			"",
			client.t("Here are the commands you can use:"),
		}...)
		// show general help
		var shownHelpLines sort.StringSlice
		var disabledCommands bool
		for _, commandInfo := range service.Commands {
			// skip commands user can't access
			if 0 < len(commandInfo.capabs) && !client.HasRoleCapabs(commandInfo.capabs...) {
				continue
			}
			if commandInfo.aliasOf != "" || commandInfo.hidden {
				continue // don't show help lines for aliases
			}
			if commandInfo.enabled != nil && !commandInfo.enabled(config) {
				disabledCommands = true
				continue
			}

			shownHelpLines = append(shownHelpLines, ircfmt.Unescape("    "+client.t(commandInfo.helpShort)))
		}

		if disabledCommands {
			shownHelpLines = append(shownHelpLines, "    "+client.t("... and other commands which have been disabled"))
		}

		// sort help lines
		sort.Sort(shownHelpLines)

		// push out help text
		for _, line := range helpBannerLines {
			sendNotice(line)
		}
		for _, line := range shownHelpLines {
			sendNotice(line)
		}
	} else {
		commandName := strings.ToLower(params[0])
		commandInfo := lookupServiceCommand(service.Commands, commandName)
		if commandInfo == nil {
			sendNotice(client.t(fmt.Sprintf("Unknown command. To see available commands, run /%s HELP", service.ShortName)))
		} else {
			helpStrings := commandInfo.helpStrings
			if helpStrings == nil {
				hsArray := [1]string{commandInfo.help}
				helpStrings = hsArray[:]
			}
			for i, helpString := range helpStrings {
				if 0 < i {
					sendNotice("")
				}
				for _, line := range strings.Split(ircfmt.Unescape(client.t(helpString)), "\n") {
					sendNotice(line)
				}
			}
		}
	}

	sendNotice(ircfmt.Unescape(fmt.Sprintf(client.t("*** $bEnd of %s HELP$b ***"), service.Name)))
}

func makeServiceHelpTextGenerator(cmd string, banner string) func(*Client) string {
	return func(client *Client) string {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, client.t("%s <subcommand> [params]"), cmd)
		buf.WriteRune('\n')
		buf.WriteString(client.t(banner)) // may contain newlines, that's fine
		buf.WriteRune('\n')
		fmt.Fprintf(&buf, client.t("For more details, try /%s HELP"), cmd)
		return buf.String()
	}
}

func initializeServices() {
	// this modifies the global Commands map,
	// so it must be called from irc/commands.go's init()
	oragonoServicesByCommandAlias = make(map[string]*ircService)

	for serviceName, service := range OragonoServices {
		service.prefix = fmt.Sprintf("%s!%s@localhost", service.Name, service.Name)

		// make `/MSG ServiceName HELP` work correctly
		service.Commands["help"] = &servHelpCmd

		// reserve the nickname
		restrictedNicknames = append(restrictedNicknames, service.Name)

		// register the protocol-level commands (NICKSERV, NS) that talk to the service,
		// and their associated help entries
		var ircCmdDef Command
		ircCmdDef.handler = serviceCmdHandler
		for _, ircCmd := range service.CommandAliases {
			Commands[ircCmd] = ircCmdDef
			oragonoServicesByCommandAlias[ircCmd] = service
			Help[strings.ToLower(ircCmd)] = HelpEntry{
				textGenerator: makeServiceHelpTextGenerator(ircCmd, service.HelpBanner),
			}
		}

		// force devs to write a help entry for every command
		for commandName, commandInfo := range service.Commands {
			if commandInfo.aliasOf == "" && !commandInfo.hidden {
				if (commandInfo.help == "" && commandInfo.helpStrings == nil) || commandInfo.helpShort == "" {
					log.Fatal(fmt.Sprintf("help entry missing for %s command %s", serviceName, commandName))
				}
			}

			if commandInfo.maxParams == 0 && commandInfo.unsplitFinalParam {
				log.Fatal("unsplitFinalParam requires use of maxParams")
			}
		}
	}

	for _, restrictedNickname := range restrictedNicknames {
		cfName, err := CasefoldName(restrictedNickname)
		if err != nil {
			panic(err)
		}
		restrictedCasefoldedNicks[cfName] = true
		skeleton, err := Skeleton(restrictedNickname)
		if err != nil {
			panic(err)
		}
		restrictedSkeletons[skeleton] = true
	}
}
