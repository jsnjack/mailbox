package ui

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

type recipientField struct {
	box        *gtk.Box
	entry      *gtk.Entry
	chips      *gtk.FlowBox
	chipScroll *gtk.ScrolledWindow
	errorLabel *gtk.Label
	addresses  []string
	changed    func()
	updating   bool
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
	parts := append([]string(nil), f.addresses...)
	if pending := strings.Trim(f.entry.Text(), ", \t\r\n"); pending != "" {
		parts = append(parts, pending)
	}
	return strings.Join(parts, ", ")
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

func (f *recipientField) commit() bool {
	parsed, err := parseRecipients(f.entry.Text())
	if err != nil {
		f.notify()
		return false
	}
	if len(parsed) == 0 {
		return true
	}
	f.addresses = append(f.addresses, parsed...)
	f.updating = true
	f.entry.SetText("")
	f.updating = false
	f.render()
	f.notify()
	return true
}

func (f *recipientField) render() {
	for child := f.chips.FirstChild(); child != nil; child = f.chips.FirstChild() {
		f.chips.Remove(child)
	}
	f.chips.SetVisible(len(f.addresses) > 0)
	if f.chipScroll != nil {
		f.chipScroll.SetVisible(len(f.addresses) > 0)
	}
	for i, address := range f.addresses {
		chip := gtk.NewBox(gtk.OrientationHorizontal, 0)
		chip.AddCSSClass("linked")
		edit := gtk.NewButton()
		label := gtk.NewLabel(address)
		label.SetEllipsize(pango.EllipsizeMiddle)
		label.SetMaxWidthChars(26)
		edit.SetChild(label)
		edit.SetTooltipText("Edit " + address)
		a11yLabel(edit, "Edit recipient "+address)
		edit.ConnectClicked(func() {
			if !f.commit() {
				return
			}
			f.addresses = append(f.addresses[:i], f.addresses[i+1:]...)
			f.updating = true
			f.entry.SetText(address)
			f.updating = false
			f.render()
			f.notify()
			f.entry.GrabFocus()
			f.entry.SelectRegion(0, -1)
		})
		remove := gtk.NewButtonFromIconName("window-close-symbolic")
		a11yLabel(remove, "Remove recipient "+address)
		remove.SetTooltipText("Remove " + address)
		remove.ConnectClicked(func() { f.addresses = append(f.addresses[:i], f.addresses[i+1:]...); f.render(); f.notify() })
		chip.Append(edit)
		chip.Append(remove)
		f.chips.Append(chip)
	}
}

func newRecipientField(name, initial string) *recipientField {
	f := &recipientField{box: gtk.NewBox(gtk.OrientationVertical, 3), entry: gtk.NewEntry(), chips: gtk.NewFlowBox(), errorLabel: gtk.NewLabel("")}
	f.box.SetHExpand(true)
	f.entry.SetPlaceholderText(name)
	f.entry.SetHExpand(true)
	a11yLabel(f.entry, name+" recipients")
	f.chips.SetSelectionMode(gtk.SelectionNone)
	f.chips.SetColumnSpacing(3)
	f.chips.SetRowSpacing(3)
	f.chips.SetMaxChildrenPerLine(8)
	f.errorLabel.SetXAlign(0)
	f.errorLabel.SetWrap(true)
	f.errorLabel.AddCSSClass("error")
	f.errorLabel.AddCSSClass("caption")
	f.chipScroll = gtk.NewScrolledWindow()
	f.chipScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	f.chipScroll.SetMaxContentHeight(96)
	f.chipScroll.SetPropagateNaturalHeight(true)
	f.chipScroll.SetChild(f.chips)
	f.box.Append(f.chipScroll)
	f.box.Append(f.entry)
	f.box.Append(f.errorLabel)
	f.entry.SetText(initial)
	f.commit()
	f.render()
	f.notify()
	f.entry.ConnectChanged(func() {
		if f.updating {
			return
		}
		if strings.HasSuffix(strings.TrimSpace(f.entry.Text()), ",") {
			if f.commit() {
				return
			}
		}
		f.notify()
	})
	f.entry.ConnectActivate(func() { f.commit() })
	focus := gtk.NewEventControllerFocus()
	focus.ConnectLeave(func() { f.commit() })
	f.entry.AddController(focus)
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
