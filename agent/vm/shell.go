package vm

import (
	"maps"
	"slices"
	"strings"
)

// quote makes word one literal POSIX shell word. Everything inside single quotes is literal, so each embedded single
// quote closes the quoting, adds an escaped quote and reopens it.
func quote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func quoteAll(words []string) string {
	quoted := make([]string, len(words))
	for index, word := range words {
		quoted[index] = quote(word)
	}
	return strings.Join(quoted, " ")
}

// execScript is the guest script for one command. It is sent on ssh's standard input to "/bin/sh -s", so neither the
// environment nor the arguments appear in the host's process table. It records the command's process group in
// groupFile, so cancelling can signal the whole tree, then replaces the shell with the command.
func execScript(groupFile string, env map[string]string, args []string) string {
	var script strings.Builder
	script.WriteString("set -e\n")
	script.WriteString("ps -o pgid= -p $$ >" + quote(groupFile) + "\n")
	for _, key := range slices.Sorted(maps.Keys(env)) {
		script.WriteString("export " + quote(key+"="+env[key]) + "\n")
	}
	script.WriteString("exec " + quoteAll(args) + "\n")
	return script.String()
}

// killScript terminates the process group recorded in groupFile, escalating to SIGKILL after about ten seconds. It
// writes the group as "-ID" with no "--", the one form dash, bash and zsh all parse.
func killScript(groupFile string) string {
	return `group=$(tr -d ' \n' <` + quote(groupFile) + `) || exit 0
[ -n "$group" ] || exit 0
kill -TERM "-$group" 2>/dev/null || exit 0
attempt=0
while kill -0 "-$group" 2>/dev/null; do
	attempt=$((attempt + 1))
	if [ "$attempt" -gt 10 ]; then
		kill -KILL "-$group" 2>/dev/null || true
		exit 0
	fi
	sleep 1
done
`
}

// prepareScript creates the parent of an incoming copy's destination and refuses an existing directory there.
func prepareScript(parent string, destination string) string {
	return "set -e\nmkdir -p -- " + quote(parent) + "\nif [ -d " + quote(destination) + " ]; then exit 3; fi\n"
}

const homeScript = `printf '%s' "$HOME"` + "\n"
