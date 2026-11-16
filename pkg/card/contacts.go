package card

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/prometheus/alertmanager/template"
	"go.yaml.in/yaml/v3"
)

// The contacts file (-contacts-file) says who to name on a card: the people
// behind each value of one alert label, the owner of what the alert is about
// and their backup. A card names them; for a firing alert of a severity in
// mention_severities it @mentions them, and Teams notifies them.
//
//	label: owner              # the alert label that names the group; default "owner"
//	default: platform         # the group of an alert without that label, or with
//	                          # a value not listed below; optional
//	mention_severities: [critical]   # default [critical]; [] never mentions
//	groups:
//	  platform:
//	    primary: [{name: Jane Doe, id: jane@example.com}]
//	    backup:  [{name: John Roe, id: john@example.com}]
//
// id is what Teams knows the person by: the user principal name (the Teams
// sign-in name) or the Microsoft Entra object id. name is what the card shows.

// defaultContactLabel is the alert label read when the file names none.
const defaultContactLabel = "owner"

// defaultMentionSeverities are the severities mentioned when the file does
// not say.
var defaultMentionSeverities = []string{"critical"}

// Person is someone a card names.
type Person struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
}

// ContactGroup is the people behind one label value: who owns it (primary)
// and who stands in for them (backup).
type ContactGroup struct {
	Primary []Person `yaml:"primary"`
	Backup  []Person `yaml:"backup"`
}

// Contacts is the contacts file. A nil *Contacts is valid: no file, nobody
// is named.
type Contacts struct {
	Label             string                  `yaml:"label"`
	Default           string                  `yaml:"default"`
	MentionSeverities []string                `yaml:"mention_severities"`
	Groups            map[string]ContactGroup `yaml:"groups"`
}

// AlertContacts is what a card gets for its alert group: the people to name,
// primary and backup, each person once, and whether to @mention them.
type AlertContacts struct {
	Mention bool
	Primary []Person
	Backup  []Person
}

// LoadContacts reads and checks a contacts file. An unknown key is an error,
// so that a typo does not silently leave people out.
func LoadContacts(path string) (*Contacts, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path comes from the command line
	if err != nil {
		return nil, fmt.Errorf("cannot read the contacts file: %w", err)
	}
	var c Contacts
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("contacts file %s: %w", path, err)
	}
	if c.Label == "" {
		c.Label = defaultContactLabel
	}
	// Left out: the default. Written as []: never mention.
	if c.MentionSeverities == nil {
		c.MentionSeverities = defaultMentionSeverities
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("contacts file %s: %w", path, err)
	}
	return &c, nil
}

// validate checks that every group has someone in it, every person a name
// and an id, and that the default group exists.
func (c *Contacts) validate() error {
	if len(c.Groups) == 0 {
		return errors.New("no groups")
	}
	for key, g := range c.Groups {
		if len(g.Primary)+len(g.Backup) == 0 {
			return fmt.Errorf("group %q has nobody in it", key)
		}
		for _, p := range slices.Concat(g.Primary, g.Backup) {
			if p.Name == "" || p.ID == "" {
				return fmt.Errorf("group %q: every person needs a name and an id", key)
			}
		}
	}
	if c.Default != "" {
		if _, ok := c.Groups[c.Default]; !ok {
			return fmt.Errorf("default %q is not one of the groups", c.Default)
		}
	}
	return nil
}

// For returns the people to name on the card of an alert group, or nil when
// there is nobody (no contacts file, or no group for any of the alerts).
//
// The people come from the firing alerts; a resolved group, which has none,
// from all its alerts. Each alert's group is found by its label (else the
// default group); a person named by several groups is named once, as
// primary if any group has them as primary. Mention is true when one of the
// firing alerts has a severity of mention_severities: a resolved card names
// the people without notifying them again.
func (c *Contacts) For(alerts template.Alerts) *AlertContacts {
	if c == nil {
		return nil
	}
	from := alerts.Firing()
	firing := len(from) > 0
	if !firing {
		from = alerts
	}

	var out AlertContacts
	var backup []Person
	seen := map[string]bool{}
	for _, a := range from {
		g, ok := c.group(a.Labels[c.Label])
		if !ok {
			continue
		}
		if firing && slices.Contains(c.MentionSeverities, a.Labels["severity"]) {
			out.Mention = true
		}
		for _, p := range g.Primary {
			if !seen[p.ID] {
				seen[p.ID] = true
				out.Primary = append(out.Primary, p)
			}
		}
		backup = append(backup, g.Backup...)
	}
	for _, p := range backup {
		if !seen[p.ID] {
			seen[p.ID] = true
			out.Backup = append(out.Backup, p)
		}
	}
	if len(out.Primary)+len(out.Backup) == 0 {
		return nil
	}
	return &out
}

// group returns the group of a label value: its own, else the default one.
func (c *Contacts) group(value string) (ContactGroup, bool) {
	if g, ok := c.Groups[value]; ok {
		return g, true
	}
	g, ok := c.Groups[c.Default]
	return g, ok && c.Default != ""
}
