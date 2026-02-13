// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package config describes the connection settings for Proxmox infra provider.
package config

// Config describes Proxmox provider configuration.
type Config struct {
	Proxmox Proxmox `yaml:"proxmox"`
}

// Proxmox is the config for accessing Proxmox API.
type Proxmox struct {
	URL string `yaml:"url"`

	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Realm    string `yaml:"realm,omitempty"`

	TokenID     string `yaml:"tokenID,omitempty"`
	TokenSecret string `yaml:"tokenSecret,omitempty"`

	InsecureSkipVerify bool `yaml:"insecureSkipVerify,omitempty"`
}
