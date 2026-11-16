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
//	  storage:
//	    primary: [{name: Ann Lee, id: ann@example.com, mention: false, note: Storage team - contact personally}]
//	    responders: platform
//
// id is what Teams knows the person by: the user principal name (the Teams
// sign-in name) or the Microsoft Entra object id. name is what the card shows.
// mention: false names a person without mentioning them - someone who does not
// read the channel, reached another way; note is shown after their role.
// responders names another group whose people react to the alert and are
// mentioned instead: "responders: @Jane Doe, @John Roe".

// defaultContactLabel is the alert label read when the file names none.
const defaultContactLabel = "owner"

// defaultMentionSeverities are the severities mentioned when the file does
// not say.
var defaultMentionSeverities = []string{"critical"}

// Person is someone a card names.
type Person struct {
	Name string `yaml:"name"`
	ID   string `yaml:"id"`
	// Note is shown after the person's role: "(primary, <note>)".
	Note string `yaml:"note"`
	// Mention false: named, never mentioned. Left out: true. (A pointer,
	// so that "left out" and "false" can be told apart.)
	Mention *bool `yaml:"mention"`
}

// Mentionable reports whether the person may be @mentioned.
func (p Person) Mentionable() bool {
	return p.Mention == nil || *p.Mention
}

// ContactGroup is the people behind one label value: who owns it (primary),
// who stands in for them (backup), and optionally the group whose people
// react to its alerts (responders).
type ContactGroup struct {
	Primary    []Person `yaml:"primary"`
	Backup     []Person `yaml:"backup"`
	Responders string   `yaml:"responders"`
}

// Contacts is the contacts file. A nil *Contacts is valid: no file, nobody
// is named.
type Contacts struct {
	Label             string                  `yaml:"label"`
	Default           string                  `yaml:"default"`
	MentionSeverities []string                `yaml:"mention_severities"`
	Groups            map[string]ContactGroup `yaml:"groups"`
}

// AlertContacts is what a card gets for its alert group: the people to name -
// primary, backup and responders, each person once - whether to @mention
// them (each person only if Mentionable), and the alert label that names
// their group (the card shows it next to them).
type AlertContacts struct {
	Label      string
	Mention    bool
	Primary    []Person
	Backup     []Person
	Responders []Person
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
// and an id, that the default group and every responders group exist, and
// that a responders group has no responders of its own (they are not
// followed further).
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
		if g.Responders != "" {
			r, ok := c.Groups[g.Responders]
			if !ok {
				return fmt.Errorf("group %q: responders %q is not one of the groups", key, g.Responders)
			}
			if r.Responders != "" {
				return fmt.Errorf("group %q: responders %q has responders of its own", key, g.Responders)
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
// default group). A person is named once, in the first of primary, backup,
// responders that any of the groups gives them. Mention is true when one of
// the firing alerts has a severity of mention_severities: a resolved card
// names the people without notifying them again.
func (c *Contacts) For(alerts template.Alerts) *AlertContacts {
	if c == nil {
		return nil
	}
	from := alerts.Firing()
	firing := len(from) > 0
	if !firing {
		from = alerts
	}

	out := AlertContacts{Label: c.Label}
	var primary, backup, responders []Person
	for _, a := range from {
		g, ok := c.group(a.Labels[c.Label])
		if !ok {
			continue
		}
		if firing && slices.Contains(c.MentionSeverities, a.Labels["severity"]) {
			out.Mention = true
		}
		primary = append(primary, g.Primary...)
		backup = append(backup, g.Backup...)
		if r, ok := c.Groups[g.Responders]; ok && g.Responders != "" {
			responders = slices.Concat(responders, r.Primary, r.Backup)
		}
	}
	seen := map[string]bool{}
	once := func(people []Person) []Person {
		var kept []Person
		for _, p := range people {
			if !seen[p.ID] {
				seen[p.ID] = true
				kept = append(kept, p)
			}
		}
		return kept
	}
	out.Primary, out.Backup, out.Responders = once(primary), once(backup), once(responders)
	if len(out.Primary)+len(out.Backup)+len(out.Responders) == 0 {
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

// WithMentionSeverities returns a copy of c that mentions the given
// severities ([] never), for one channel that differs from the file - a test
// channel nobody must be woken by. A nil c stays nil.
func (c *Contacts) WithMentionSeverities(severities []string) *Contacts {
	if c == nil {
		return nil
	}
	copied := *c
	copied.MentionSeverities = slices.Clone(severities)
	return &copied
}
