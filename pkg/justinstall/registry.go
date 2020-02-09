package justinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/gotopkg/mslnk/pkg/mslnk"
	"github.com/just-install/just-install/pkg/cmd"
	"github.com/just-install/just-install/pkg/fetch"
	"github.com/just-install/just-install/pkg/installer"
	"github.com/just-install/just-install/pkg/paths"
	dry "github.com/ungerik/go-dry"
)

const registrySupportedVersion = 4

var (
	shimsPath = os.ExpandEnv("${SystemDrive}\\Shims")
	startMenu = os.ExpandEnv("${ProgramData}\\Microsoft\\Windows\\Start Menu\\Programs")
)

//
// Public
//

// LoadRegistry unmarshals the registry from a local file path.
func LoadRegistry(path string) Registry {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatalf("Unable to read the registry file.")
	}

	var ret Registry

	if err := json.Unmarshal(data, &ret); err != nil {
		log.Fatalln("Unable to parse the registry file.")
	}

	if ret.Version != registrySupportedVersion {
		log.Fatalln("Please update to a new version of just-install by running: msiexec.exe /i https://just-install.github.io/stable/just-install.msi")
	}

	return ret
}

//
// Installer Entry
//

type installerEntry struct {
	Interactive bool
	Kind        string
	Options     map[string]interface{} // Optional
	X86         string
	X86_64      string
}

// options returns the architecture-specific options (if available), otherwise returns the whole
// options map.
func (s *installerEntry) options(arch string) map[string]interface{} {
	archSpecificOptions, ok := s.Options[arch].(map[string]interface{})
	if !ok {
		return s.Options
	}

	return archSpecificOptions
}

//
// Registry
//

// Registry is a list of packages that just-install knows how to install.
type Registry struct {
	Version  int
	Packages map[string]RegistryEntry
}

// SortedPackageNames returns the list of packages present in the registry, sorted alphabetically.
func (r *Registry) SortedPackageNames() []string {
	var keys []string

	for k := range r.Packages {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// RegistryEntry is a single entry in the just-install registry.
type RegistryEntry struct {
	Version   string
	Installer installerEntry
	SkipAudit bool
}

// DownloadInstaller downloads the installer for the current entry in the temporary directory.
func (e *RegistryEntry) DownloadInstaller(arch string, force bool) string {
	url, err := e.installerURL(arch)
	if err != nil {
		// FIXME: Add proper error handling
		log.Fatalln("Cannot determine installer URL:", err)
	}

	downloadDir, err := paths.TempDirCreate()
	if err != nil {
		// FIXME: Add proper error handling
		log.Fatalln("Could not create temporary directory:", err)
	}

	ret, err := fetch.Fetch(url, &fetch.Options{Destination: downloadDir, Progress: true})
	if err != nil {
		log.Fatalln(err)
	}

	return ret
}

// JustInstall will download and install the given registry entry. Setting `force` to true will
// force a re-download and re-installation the package.
func (e *RegistryEntry) JustInstall(arch string, force bool) error {
	options := e.Installer.options(arch)
	downloadedFile := e.DownloadInstaller(arch, force)

	if container, ok := options["container"]; ok {
		tempDir, err := paths.TempDirCreate()
		if err != nil {
			return err
		}
		tempDir = filepath.Join(tempDir, filepath.Base(downloadedFile)+"_extracted")

		if err := installer.ExtractZIP(downloadedFile, tempDir); err != nil {
			return err
		}

		installer := container.(map[string]interface{})["installer"].(string)
		if err := e.install(arch, filepath.Join(tempDir, installer)); err != nil {
			return err
		}
	} else {
		if err := e.install(arch, downloadedFile); err != nil {
			return err
		}
	}

	e.CreateShims(arch)

	return nil
}

func (e *RegistryEntry) installerURL(arch string) (string, error) {
	var url string

	if arch == "x86_64" {
		if e.Installer.X86_64 != "" {
			url = e.Installer.X86_64
		} else if e.Installer.X86 != "" {
			url = e.Installer.X86
		} else {
			return "", errors.New("No fallback 32-bit download")
		}
	} else if arch == "x86" {
		if e.Installer.X86 != "" {
			url = e.Installer.X86
		} else {
			return "", errors.New("64-bit only package")
		}
	} else {
		return "", errors.New("Unknown architecture")
	}

	return e.ExpandString(url), nil
}

func (e *RegistryEntry) ExpandString(s string) string {
	return expandString(s, map[string]string{"version": e.Version})
}

func (e *RegistryEntry) install(arch string, path string) error {
	// One-off, custom, installers
	switch e.Installer.Kind {
	case "copy":
		destination := e.destination(arch)

		parentDir := filepath.Dir(destination)
		log.Println("Creating", parentDir)
		if err := os.MkdirAll(parentDir, os.ModePerm); err != nil {
			return err
		}

		log.Println("Copying to", destination)
		return dry.FileCopy(path, destination)
	case "custom":
		var args []string

		for _, v := range e.Installer.options(arch)["arguments"].([]interface{}) {
			args = append(args, expandString(v.(string), map[string]string{"installer": path}))
		}

		return cmd.Run(args...)
	case "zip":
		log.Println("Extracting to", e.destination(arch))

		if err := installer.ExtractZIP(path, e.destination(arch)); err != nil {
			return err
		}

		if shortcuts, prs := e.Installer.options(arch)["shortcuts"]; prs {
			for _, shortcut := range shortcuts.([]interface{}) {
				shortcutName := expandString(shortcut.(map[string]interface{})["name"].(string), nil)
				shortcutTarget := expandString(os.ExpandEnv(shortcut.(map[string]interface{})["target"].(string)), nil)
				shortcutLocation := filepath.Join(startMenu, shortcutName+".lnk")

				log.Println("Creating shortcut to", shortcutTarget, "in", shortcutLocation)

				if err := mslnk.LinkFile(shortcutTarget, shortcutLocation); err != nil {
					return err
				}
			}
		}

		return nil
	}

	// Regular installer
	installerType := installer.InstallerType(e.Installer.Kind)
	if !installerType.IsValid() {
		return fmt.Errorf("unknown installer type: %v", e.Installer.Kind)
	}

	installerCommand, err := installer.Command(path, installerType)
	if err != nil {
		return err
	}

	return cmd.Run(installerCommand...)
}

func (e *RegistryEntry) destination(arch string) string {
	return expandString(os.ExpandEnv(e.Installer.options(arch)["destination"].(string)), nil)
}

func (e *RegistryEntry) CreateShims(arch string) {
	exeproxy := os.ExpandEnv("${ProgramFiles(x86)}\\exeproxy\\exeproxy.exe")
	if !dry.FileExists(exeproxy) {
		return
	}

	if !dry.FileIsDir(shimsPath) {
		if err := os.MkdirAll(shimsPath, 0); err != nil {
			// FIXME: add proper error handling
			log.Fatalln("Could not create shim directory:", err)
		}
	}

	if shims, ok := e.Installer.options(arch)["shims"]; ok {
		for _, v := range shims.([]interface{}) {
			shimTarget := e.ExpandString(v.(string))
			shim := filepath.Join(shimsPath, filepath.Base(shimTarget))

			if dry.FileExists(shim) {
				os.Remove(shim)
			}

			log.Printf("Creating shim for %s (%s)\n", shimTarget, shim)

			if err := cmd.Run(exeproxy, "exeproxy-copy", shim, shimTarget); err != nil {
				// FIXME: add proper error handling
				log.Fatalln("Could not create shim:", err)
			}
		}
	}
}
