package channelagent

import "strings"

type Command struct {
	Name  string
	Args  []string
	Flags map[string]bool
}

// ParseCommand parses a control message. Returns ok=false for non-command text
// (anything not starting with "/"). Tokens of the form --flag become Flags;
// everything else is a positional Arg.
func ParseCommand(content string) (Command, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "/") {
		return Command{}, false
	}
	fields := strings.Fields(content[1:])
	if len(fields) == 0 {
		return Command{}, false
	}
	cmd := Command{Name: fields[0], Flags: map[string]bool{}}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "--") {
			cmd.Flags[strings.TrimPrefix(f, "--")] = true
			continue
		}
		cmd.Args = append(cmd.Args, f)
	}
	return cmd, true
}
