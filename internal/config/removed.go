// -------------------------------------------------------------------------------
// Configuration - Removed Keys
//
// Author: Alex Freidah
//
// Reports configuration this release no longer accepts. The keys are gone from
// the structs, and YAML ignores what it cannot map, so without this a config
// still carrying one would start cleanly and behave differently from what it
// says - an operator would believe a token still guards a surface that no longer
// reads it.
//
// Each key is named alongside what replaces it, because the fix is never to
// delete the line: it is to declare a credential instead.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// removedConfig captures only the keys this release refuses. It is unmarshalled
// from the same document as Config, which is what lets a key that no longer maps
// to a field still be seen.
type removedConfig struct {
	UI struct {
		AdminToken  string `yaml:"admin_token"`
		AdminKey    string `yaml:"admin_key"`
		AdminSecret string `yaml:"admin_secret"`
	} `yaml:"ui"`
	Buckets []struct {
		Name        string `yaml:"name"`
		Credentials []struct {
			Token string `yaml:"token"`
		} `yaml:"credentials"`
	} `yaml:"buckets"`
}

// checkRemovedKeys reports every removed key a document still carries.
//
// All of them are reported rather than the first, so an operator migrating a
// config edits it once instead of rerunning to discover the next one.
func checkRemovedKeys(doc []byte) []error {
	var r removedConfig
	// A document that does not parse fails on the real unmarshal with a better
	// message than anything this could add.
	if err := yaml.Unmarshal(doc, &r); err != nil {
		return nil
	}

	var errs []error
	if r.UI.AdminToken != "" {
		errs = append(errs, ErrRemovedAdminToken)
	}
	if r.UI.AdminKey != "" || r.UI.AdminSecret != "" {
		errs = append(errs, ErrRemovedAdminLogin)
	}
	for i := range r.Buckets {
		b := &r.Buckets[i]
		for j := range b.Credentials {
			if b.Credentials[j].Token != "" {
				errs = append(errs, fmt.Errorf("%w: bucket %q", ErrRemovedProxyToken, b.Name))
				break
			}
		}
	}
	return errs
}
