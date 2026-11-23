package connector

import (
	"encoding/base64"
	"html"
	"strconv"
	"strings"
	"time"
)

// RawMIMEHeader represents a single MIME header entry in a raw Gmail/email JSON payload.
type RawMIMEHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RawMIMEBody holds inline base64url body data or an attachment reference.
type RawMIMEBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"`
}

// RawMIMEPart represents a node in a raw RFC 5322 MIME tree.
type RawMIMEPart struct {
	MimeType string          `json:"mimeType"`
	Filename string          `json:"filename"`
	Body     RawMIMEBody     `json:"body"`
	Headers  []RawMIMEHeader `json:"headers"`
	Parts    []RawMIMEPart   `json:"parts"`
}

// RawMailMessage represents a raw Gmail API / plugin CLI / mock email message payload.
type RawMailMessage struct {
	ID           string      `json:"id"`
	ThreadID     string      `json:"threadId"`
	Snippet      string      `json:"snippet"`
	InternalDate string      `json:"internalDate"`
	From         string      `json:"from"`
	To           string      `json:"to"`
	Cc           string      `json:"cc"`
	Subject      string      `json:"subject"`
	Date         string      `json:"date"`
	PlainBody    string      `json:"plainBody"`
	HTMLBody     string      `json:"htmlBody"`
	AuthResults  string      `json:"authResults"`
	LabelIDs     []string    `json:"labelIds"`
	Payload      RawMIMEPart `json:"payload"`
}

// RawAttachmentMeta describes an attachment discovered while walking a RawMIMEPart tree.
type RawAttachmentMeta struct {
	AttachmentID string `json:"attachmentId"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
}

// ParsedRawMail holds normalized headers, bodies, timestamp, and attachments extracted from a RawMailMessage.
type ParsedRawMail struct {
	ID          string
	ThreadID    string
	From        string
	To          string
	Cc          string
	Subject     string
	Date        string
	Timestamp   string
	Snippet     string
	AuthResults string
	Labels      []string
	PlainBody   string
	HTMLBody    string
	Attachments []RawAttachmentMeta
}

// DecodeBase64URL decodes a base64url string (trying unpadded then padded encodings), returning "" on error.
func DecodeBase64URL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return ""
}

// ExtractMIMEBodies walks p in pre-order to extract the first text/plain body, first text/html body, and all attachment metadata.
func ExtractMIMEBodies(p RawMIMEPart) (plainOut, htmlOut string, attachments []RawAttachmentMeta) {
	var walk func(part RawMIMEPart)
	walk = func(part RawMIMEPart) {
		mt := strings.ToLower(part.MimeType)
		if part.Filename != "" && part.Body.AttachmentID != "" {
			attachments = append(attachments, RawAttachmentMeta{
				AttachmentID: part.Body.AttachmentID,
				Filename:     part.Filename,
				MimeType:     part.MimeType,
				Size:         part.Body.Size,
			})
		}
		if mt == "text/plain" && part.Body.Data != "" && plainOut == "" {
			plainOut = DecodeBase64URL(part.Body.Data)
		} else if mt == "text/html" && part.Body.Data != "" && htmlOut == "" {
			htmlOut = DecodeBase64URL(part.Body.Data)
		}
		for _, sub := range part.Parts {
			walk(sub)
		}
	}
	walk(p)
	return plainOut, htmlOut, attachments
}

// ParseRawMail normalizes a RawMailMessage into a ParsedRawMail using first-header-wins precedence.
func ParseRawMail(rm RawMailMessage) ParsedRawMail {
	id := strings.TrimSpace(rm.ID)
	threadID := strings.TrimSpace(rm.ThreadID)
	if threadID == "" {
		threadID = id
	}
	from := rm.From
	to := rm.To
	cc := rm.Cc
	subject := rm.Subject
	date := rm.Date
	authRes := rm.AuthResults
	for _, h := range rm.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			if from == "" {
				from = h.Value
			}
		case "to":
			if to == "" {
				to = h.Value
			}
		case "cc":
			if cc == "" {
				cc = h.Value
			}
		case "subject":
			if subject == "" {
				subject = h.Value
			}
		case "date":
			if date == "" {
				date = h.Value
			}
		case "authentication-results":
			if authRes == "" {
				authRes = h.Value
			}
		}
	}
	plainB, htmlB, atts := ExtractMIMEBodies(rm.Payload)
	if rm.PlainBody != "" {
		plainB = rm.PlainBody
	}
	if rm.HTMLBody != "" {
		htmlB = rm.HTMLBody
	}
	ts := date
	if rm.InternalDate != "" {
		if ms, err := strconv.ParseInt(rm.InternalDate, 10, 64); err == nil && ms > 0 {
			ts = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
	}
	return ParsedRawMail{
		ID:          id,
		ThreadID:    threadID,
		From:        from,
		To:          to,
		Cc:          cc,
		Subject:     subject,
		Date:        date,
		Timestamp:   ts,
		Snippet:     html.UnescapeString(rm.Snippet),
		AuthResults: authRes,
		Labels:      rm.LabelIDs,
		PlainBody:   plainB,
		HTMLBody:    htmlB,
		Attachments: atts,
	}
}
