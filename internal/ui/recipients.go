package ui

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

type recipientField struct {
	box        *gtk.Box
	entry      *gtk.Entry
	errorLabel *gtk.Label
	changed    func()
}

func parseRecipients(text string) ([]string, error) {
	text = strings.Trim(text, ", \t\r\n")
	if text == "" {
		return nil, nil
	}
	parsed, err := mail.ParseAddressList(text)
	if err != nil {
		return nil, fmt.Errorf("enter valid email addresses separated by commas")
	}
	result := make([]string, 0, len(parsed))
	for _, address := range parsed {
		at := strings.LastIndexByte(address.Address, '@')
		if at <= 0 || at == len(address.Address)-1 {
			return nil, fmt.Errorf("enter a complete email address")
		}
		if address.Name == "" {
			result = append(result, address.Address)
		} else {
			result = append(result, address.String())
		}
	}
	return result, nil
}

func (f *recipientField) text() string {
	return strings.Trim(f.entry.Text(), ", \t\r\n")
}

func (f *recipientField) notify() {
	_, err := parseRecipients(f.text())
	f.errorLabel.SetVisible(err != nil)
	if err != nil {
		f.errorLabel.SetText(err.Error())
		f.entry.AddCSSClass("error")
	} else {
		f.entry.RemoveCSSClass("error")
	}
	if f.changed != nil {
		f.changed()
	}
}

func newRecipientField(name, initial string) *recipientField {
	f := &recipientField{box: gtk.NewBox(gtk.OrientationVertical, 3), entry: gtk.NewEntry(), errorLabel: gtk.NewLabel("")}
	f.box.SetHExpand(true)
	f.box.SetVExpand(false)
	f.box.SetVAlign(gtk.AlignStart)
	f.entry.SetHExpand(true)
	f.entry.SetPlaceholderText(name)
	f.entry.SetText(initial)
	a11yLabel(f.entry, name+" recipients")
	f.errorLabel.SetXAlign(0)
	f.errorLabel.SetWrap(true)
	f.errorLabel.AddCSSClass("error")
	f.errorLabel.AddCSSClass("caption")
	f.box.Append(f.entry)
	f.box.Append(f.errorLabel)
	f.notify()
	f.entry.ConnectChanged(f.notify)
	return f
}

func recipientSummary(fields ...string) string {
	seen := map[string]bool{}
	for _, field := range fields {
		addresses, err := parseRecipients(field)
		if err != nil {
			return "Check recipient addresses"
		}
		for _, raw := range addresses {
			address, err := mail.ParseAddress(raw)
			if err != nil {
				return "Check recipient addresses"
			}
			seen[strings.ToLower(address.Address)] = true
		}
	}
	if len(seen) == 1 {
		return "1 recipient"
	}
	return fmt.Sprintf("%d recipients", len(seen))
}

func validateRecipientFields(to, cc, bcc string) string {
	if strings.TrimSpace(to) == "" && strings.TrimSpace(cc) == "" && strings.TrimSpace(bcc) == "" {
		return "Add at least one recipient in To, Cc, or Bcc."
	}
	for _, field := range []struct{ name, text string }{{"To", to}, {"Cc", cc}, {"Bcc", bcc}} {
		if _, err := parseRecipients(field.text); err != nil {
			return fmt.Sprintf("The %s field has an invalid address. %s.", field.name, err.Error())
		}
	}
	return ""
}
