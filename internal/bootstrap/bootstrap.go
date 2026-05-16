package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anivaryam/proc-compose/internal/doctor"
)

const configFileMode = 0600

func Run(opts Options) (*Report, error) {
	doctorOpts := doctor.Options{
		Root:       opts.Root,
		ConfigFile: opts.ConfigFile,
		Write:      false,
	}
	doctorReport, err := doctor.Run(doctorOpts)
	if doctorReport == nil {
		return nil, err
	}

	report := newReport(opts, doctorReport.ConfigPath)
	report.Doctor = doctorReport

	if opts.Write {
		if doctorReport.ConfigExists && !opts.Overwrite {
			return report, fmt.Errorf("%s already exists", doctorReport.ConfigPath)
		}
		if strings.TrimSpace(doctorReport.SuggestedYAML) == "" || doctorReport.SuggestedYAML == "processes: {}\n" {
			return report, fmt.Errorf("no services detected; refusing to write empty proc-compose.yml")
		}
		message := "wrote generated proc-compose config"
		write := writeConfigExclusive
		if opts.Overwrite {
			message = "overwrote proc-compose config with generated config"
			write = writeConfigOverwrite
		}
		if err := write(doctorReport.ConfigPath, doctorReport.SuggestedYAML); err != nil {
			return report, err
		}
		report.AppliedChanges = append(report.AppliedChanges, AppliedChange{
			Path:    doctorReport.ConfigPath,
			Message: message,
		})
	}

	if opts.Verify {
		verificationPath := doctorReport.ConfigPath
		cleanup := func() {}
		if !opts.Write {
			path, cleanupFunc, tempErr := writeTempConfig(doctorReport.SuggestedYAML)
			if tempErr != nil {
				return report, tempErr
			}
			verificationPath = path
			cleanup = cleanupFunc
		}
		defer cleanup()

		verifier := opts.Verifier
		if verifier == nil {
			verifier = commandVerifier{Root: opts.Root}
		}
		verification, verifyErr := verifier.Verify(verificationPath)
		if verification != nil && verification.ConfigPath == "" {
			verification.ConfigPath = verificationPath
		}
		report.Verification = verification
		if verifyErr != nil {
			return report, verifyErr
		}
	}

	return report, nil
}

func writeConfigExclusive(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, configFileMode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(contents); err != nil {
		return err
	}
	return nil
}

func writeConfigOverwrite(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), configFileMode)
}

func writeTempConfig(contents string) (string, func(), error) {
	file, err := os.CreateTemp("", "proc-compose-bootstrap-*.yml")
	if err != nil {
		return "", func() {}, err
	}
	path := file.Name()
	if _, err := file.WriteString(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}
