package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/*.txt templates/*.html
var templateFS embed.FS

// message is one rendered email.
type message struct {
	Subject, Text, HTML string
}

// messageData is everything a template may print. The link is built by Links;
// the account name is chosen by whoever created the account, so it appears
// only in the body, escaped in the HTML part, and never in the subject, where
// it could pass for a message from Cleargate itself.
type messageData struct {
	Link        string
	AccountName string
}

// kind is one message the mailer sends: its fixed subject and its templates.
type kind struct {
	name    string
	subject string
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

// The three messages. Templates are parsed once at start-up, so a broken one
// fails the first test run rather than the first person who signs up.
var (
	kindEmailVerification = mustKind("verify_email", "Confirm your email address for Cleargate")
	kindPasswordReset     = mustKind("reset_password", "Reset your Cleargate password")
	kindInvitation        = mustKind("invitation", "You have been invited to an account on Cleargate")
)

func mustKind(name, subject string) kind {
	return kind{
		name:    name,
		subject: subject,
		text:    texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/"+name+".txt")),
		// html/template escapes by context, so an account name containing
		// markup arrives as text, and the link is only ever an attribute value.
		html: htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")),
	}
}

// render fills in one message.
func (k kind) render(d messageData) (message, error) {
	var text, html bytes.Buffer
	if err := k.text.Execute(&text, d); err != nil {
		return message{}, fmt.Errorf("mail: render %s text: %w", k.name, err)
	}
	if err := k.html.ExecuteTemplate(&html, "layout.html", d); err != nil {
		return message{}, fmt.Errorf("mail: render %s html: %w", k.name, err)
	}
	return message{Subject: k.subject, Text: text.String(), HTML: html.String()}, nil
}
