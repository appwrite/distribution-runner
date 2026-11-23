package lease

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/appwrite/distribution-runner/agent/protocol"
)

const (
	VariableWork           = "DISTRIBUTION_WORK"
	VariableIdentifier     = "DISTRIBUTION_IDENTIFIER"
	VariablePlatform       = "DISTRIBUTION_PLATFORM"
	VariableFramework      = "DISTRIBUTION_FRAMEWORK"
	VariableRootDirectory  = "DISTRIBUTION_ROOT_DIRECTORY"
	VariableInstallCommand = "DISTRIBUTION_INSTALL_COMMAND"
	VariableBuildCommand   = "DISTRIBUTION_BUILD_COMMAND"
	VariableVersionName    = "DISTRIBUTION_VERSION_NAME"
	VariableVersionCode    = "DISTRIBUTION_VERSION_CODE"
	VariableTeamID         = "DISTRIBUTION_TEAM_ID"
	VariableBuildID        = "DISTRIBUTION_BUILD_ID"
	VariableCI             = "CI"
)

const exportPrefix = "export "

var errEnvironment = errors.New("lease: malformed environment file")

// Environment returns the variables the build script sources from input/environment: the lease's environment, then
// the values the agent derives from the lease, which take precedence. Signing material is never part of it.
func Environment(lease protocol.Lease) (map[string]string, error) {
	variables := make(map[string]string, len(lease.Environment)+10)
	for name, value := range lease.Environment {
		if !identifier(name) {
			return nil, fmt.Errorf("lease: environment name %q is not a shell identifier", name)
		}
		variables[name] = value
	}
	maps.Copy(variables, map[string]string{
		VariablePlatform:       lease.Platform,
		VariableFramework:      lease.Framework,
		VariableRootDirectory:  lease.Source.RootDirectory,
		VariableInstallCommand: lease.Commands.Install,
		VariableBuildCommand:   lease.Commands.Build,
		VariableVersionName:    lease.Version.Name,
		VariableVersionCode:    strconv.Itoa(lease.Version.Code),
		VariableTeamID:         lease.Signing.TeamID,
		VariableBuildID:        lease.BuildID,
		VariableCI:             "true",
	})
	for name, value := range variables {
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("lease: environment value for %s contains NUL", name)
		}
	}
	return variables, nil
}

// EncodeEnvironment writes one `export NAME='value'` line per variable, sorted by name, with every ' written as '\”
// so that sourcing the file in a POSIX shell restores each value byte for byte.
func EncodeEnvironment(variables map[string]string) []byte {
	var builder strings.Builder
	for _, name := range slices.Sorted(maps.Keys(variables)) {
		builder.WriteString(exportPrefix)
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(quote(variables[name]))
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

// ParseEnvironment reads a file EncodeEnvironment wrote.
func ParseEnvironment(data []byte) (map[string]string, error) {
	variables := map[string]string{}
	rest := string(data)
	for rest != "" {
		line, ok := strings.CutPrefix(rest, exportPrefix)
		if !ok {
			return nil, errEnvironment
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !identifier(name) {
			return nil, errEnvironment
		}
		decoded, remainder, err := unquote(value)
		if err != nil {
			return nil, err
		}
		if rest, ok = strings.CutPrefix(remainder, "\n"); !ok {
			return nil, errEnvironment
		}
		variables[name] = decoded
	}
	return variables, nil
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func unquote(input string) (string, string, error) {
	var builder strings.Builder
	for {
		switch {
		case strings.HasPrefix(input, "'"):
			end := strings.IndexByte(input[1:], '\'')
			if end < 0 {
				return "", "", errEnvironment
			}
			builder.WriteString(input[1 : 1+end])
			input = input[2+end:]
		case strings.HasPrefix(input, `\'`):
			builder.WriteByte('\'')
			input = input[2:]
		default:
			return builder.String(), input, nil
		}
	}
}

func identifier(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		switch {
		case character == '_', 'a' <= character && character <= 'z', 'A' <= character && character <= 'Z':
		case index > 0 && '0' <= character && character <= '9':
		default:
			return false
		}
	}
	return true
}

// bundleName accepts bundle identifiers that are safe as a file name: letters, digits, '.', '-' and '_', not
// starting with '.'.
func bundleName(name string) bool {
	if name == "" || name[0] == '.' {
		return false
	}
	for _, character := range name {
		switch {
		case 'a' <= character && character <= 'z', 'A' <= character && character <= 'Z', '0' <= character && character <= '9':
		case character == '.', character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}
