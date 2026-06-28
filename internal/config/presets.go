package config

// AuthKind is how an account authenticates to IMAP/SMTP.
type AuthKind string

const (
	AuthPassword  AuthKind = "password"        // password / app password (PLAIN)
	AuthGoogle    AuthKind = "google-oauth"    // Gmail over IMAP via XOAUTH2
	AuthMicrosoft AuthKind = "microsoft-oauth" // Outlook/Office 365 via XOAUTH2
	AuthGmailREST AuthKind = "gmail-rest"      // the native Gmail REST backend
)

// Preset is a provider's connection defaults for the add-account dialog. Hosts
// and ports prefill the form; Auth selects the credential flow; Hint guides the
// user (e.g. where to create an app password). "Other" leaves everything blank
// for a manual generic-IMAP setup.
type Preset struct {
	ID   string
	Name string

	IMAPHost     string
	IMAPPort     int
	IMAPSecurity string // "tls" | "starttls" | "none"
	SMTPHost     string
	SMTPPort     int
	SMTPSecurity string

	Auth AuthKind
	Hint string
	URL  string // a "create app password" / help link, when relevant
}

// Presets are the providers offered in the add-account dialog, in display order.
// Gmail offers the native REST backend by default; GmailIMAP is the opt-in
// Gmail-over-IMAP variant.
var Presets = []Preset{
	{
		ID: "gmail", Name: "Gmail",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPSecurity: "tls",
		Auth: AuthGmailREST,
		Hint: "Sign in with Google — uses the native Gmail API (fastest, recommended).",
	},
	{
		ID: "gmail-imap", Name: "Gmail (IMAP)",
		IMAPHost: "imap.gmail.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.gmail.com", SMTPPort: 465, SMTPSecurity: "tls",
		Auth: AuthGoogle,
		Hint: "Gmail over IMAP — sign in with Google (full-mailbox access).",
	},
	{
		ID: "outlook", Name: "Outlook / Office 365",
		IMAPHost: "outlook.office365.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.office365.com", SMTPPort: 587, SMTPSecurity: "starttls",
		Auth: AuthMicrosoft,
		Hint: "Sign in with Microsoft.",
	},
	{
		ID: "yahoo", Name: "Yahoo Mail",
		IMAPHost: "imap.mail.yahoo.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.mail.yahoo.com", SMTPPort: 465, SMTPSecurity: "tls",
		Auth: AuthPassword,
		Hint: "Yahoo requires an app password (not your normal password).",
		URL:  "https://login.yahoo.com/account/security/app-passwords",
	},
	{
		ID: "icloud", Name: "iCloud Mail",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPSecurity: "starttls",
		Auth: AuthPassword,
		Hint: "iCloud requires an app-specific password.",
		URL:  "https://account.apple.com/account/manage",
	},
	{
		ID: "fastmail", Name: "Fastmail",
		IMAPHost: "imap.fastmail.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPSecurity: "tls",
		Auth: AuthPassword,
		Hint: "Fastmail requires an app password.",
		URL:  "https://www.fastmail.com/settings/security/devicekeys",
	},
	{
		ID: "other", Name: "Other (IMAP)",
		IMAPSecurity: "tls", IMAPPort: 993,
		SMTPSecurity: "tls", SMTPPort: 465,
		Auth: AuthPassword,
		Hint: "Enter your provider's IMAP and SMTP server settings.",
	},
}

// PresetByID returns the preset with the given id, or false.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}
