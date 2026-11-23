package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/appwrite/distribution-runner/agent/verify"
	"github.com/appwrite/distribution-runner/agent/vm"
)

// Drill knobs for Sample, read from the lease environment. The server rejects DISTRIBUTION_ and APPWRITE_ names.
const (
	FixtureExitCode   = "FIXTURE_EXIT_CODE"
	FixtureIdentifier = "FIXTURE_IDENTIFIER"
	FixtureSleep      = "FIXTURE_SLEEP_SECONDS"
)

const sampleImage = "appwrite/distribution-fake"

// Sample stands in for the golden image's build script, given the host directory DISTRIBUTION_WORK names. It reads
// the inputs the agent copied in and writes output/app.ipa (verify.Sample, signed for the lease's identifier, versions,
// team and profile) and output/result.json, so a lease runs end to end without a Mac. FIXTURE_EXIT_CODE fails the
// build with that status, FIXTURE_IDENTIFIER builds for another identifier so verification refuses it, and
// FIXTURE_SLEEP_SECONDS delays the build. It stops, writing nothing, as soon as ctx ends.
func Sample(ctx context.Context, work string) error {
	input := filepath.Join(work, DirectoryInput)
	data, err := os.ReadFile(filepath.Join(input, FileEnvironment))
	if err != nil {
		return err
	}
	variables, err := ParseEnvironment(data)
	if err != nil {
		return err
	}
	for _, name := range []string{FileSource, filepath.Join(DirectorySigning, FileCertificate), filepath.Join(DirectorySigning, FilePassword)} {
		if _, err := os.Stat(filepath.Join(input, name)); err != nil {
			return fmt.Errorf("sample build: missing input/%s", filepath.ToSlash(name))
		}
	}
	identifier := variables[VariableIdentifier]
	profile, err := os.ReadFile(filepath.Join(input, DirectorySigning, DirectoryProfile, identifier+ExtensionProfile))
	if err != nil {
		return fmt.Errorf("sample build: no profile for %s", identifier)
	}

	if value := variables[FixtureSleep]; value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 0 {
			return fmt.Errorf("sample build: invalid %s", FixtureSleep)
		}
		if err := sleep(ctx, time.Duration(seconds)*time.Second); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if value := variables[FixtureExitCode]; value != "" && value != "0" {
		code, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("sample build: invalid %s", FixtureExitCode)
		}
		return &vm.ExitError{Code: code}
	}
	if value := variables[FixtureIdentifier]; value != "" {
		identifier = value
	}
	versionCode, err := strconv.Atoi(variables[VariableVersionCode])
	if err != nil {
		return errors.New("sample build: version code is not an integer")
	}

	output := filepath.Join(work, DirectoryOutput)
	if err := os.MkdirAll(output, 0o700); err != nil {
		return err
	}
	err = verify.Sample(filepath.Join(output, FileIPA), verify.Spec{
		Identifier:  identifier,
		VersionName: variables[VariableVersionName],
		VersionCode: variables[VariableVersionCode],
		TeamID:      variables[VariableTeamID],
		Profile:     profile,
	})
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(map[string]any{
		"artifact":    FileIPA,
		"type":        "ipa",
		"identifier":  identifier,
		"versionName": variables[VariableVersionName],
		"versionCode": versionCode,
		"toolchain":   map[string]any{"image": sampleImage, "fastlane": nil, "xcode": nil, "flutter": nil},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(output, FileResult), manifest, 0o600)
}
