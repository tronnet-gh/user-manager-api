package app

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

const MiB = 1024 * 1024

type Config struct {
	ListenPort int `json:"listenPort"`
	PVE        struct {
		URL   string `json:"url"`
		Token struct {
			User   string `json:"user"`
			Realm  string `json:"realm"`
			ID     string `json:"id"`
			Secret string `json:"uuid"`
		}
	}
	ReloadInterval int `json:"rebuildInterval"`
}

func GetConfig(configPath string) Config {
	root, err := os.OpenRoot(".")
	if err != nil {
		log.Fatal("Error when opening root dir: ", err)
	}
	defer root.Close()

	content, err := root.ReadFile(configPath)
	if err != nil {
		log.Fatal("Error when opening config file: ", err)
	}

	var config Config
	err = json.Unmarshal(content, &config)
	if err != nil {
		log.Fatal("Error during parsing config file: ", err)
	}

	return config
}

// checks if a device pcie bus id is a super device or subsystem device
//
// subsystem devices always has the format xxxx:yy.z, whereas super devices have the format xxxx:yy
//
// returns true if BusID has format xxxx:yy
func DeviceBusIDIsSuperDevice(BusID DeviceID) bool {
	return !strings.ContainsRune(string(BusID), '.')
}

// checks if string s has one of any prefixes, and returns the prefix or "" if there was no match
//
// matches the first prefix match in array order
func AnyPrefixes(s string, prefixes []string) string {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return prefix
		}
	}

	return ""
}
