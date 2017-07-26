// Package email is an "email interface for humans." Designed to be robust and
// flexible, email aims to make sending email easy without getting in the way.
package email

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	maxLineLength = 76     // max line length per RFC 2045
	lineEnding    = "\r\n" // terminates each line

	contentDispo        = "Content-Disposition"
	contentID           = "Content-ID"
	contentXferEncoding = "Content-Transfer-Encoding"
	contentType         = "Content-Type"

	to       = "To"
	cc       = "Cc"
	bcc      = "Bcc"
	from     = "From"
	subject  = "Subject"
	date     = "Date"
	msgID    = "Message-Id"
	mimeVers = "Mime-Version"

	// defaultContentType is the default Content-Type according to RFC 2045,
	// section 5.2
	defaultContentType = "text/plain; charset=us-ascii"
)

// ErrMissingBoundary is returned when there is no boundary given for a
// multipart entity
var ErrMissingBoundary = errors.New("email: no boundary found for multipart entity")

// ErrMissingContentType is returned when there is no "Content-Type" header for
// a MIME entity
var ErrMissingContentType = errors.New("email: no Content-Type found for MIME entity")

// Email represents an RFC 5322 email.
type Email struct {
	From        string
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	Text        []byte
	HTML        []byte
	Headers     textproto.MIMEHeader
	Attachments []Attachment
}

// trimReader is a custom io.Reader that will trim any leading whitespace, as
// this can cause email imports to fail.
type trimReader struct {
	rd io.Reader
}

// Read trims off any unicode whitespace from the originating reader
func (tr trimReader) Read(buf []byte) (int, error) {
	n, err := tr.rd.Read(buf)
	// TODO: get rid of double copy
	t := bytes.TrimLeftFunc(buf[:n], unicode.IsSpace)
	n = copy(buf, t)
	return n, err
}

// NewWithSize constructs an Email from an io.Reader in the same manner as New,
// except it allows the maximum size to be specified.
func NewWithSize(r io.Reader, maxSize int64) (*Email, error) {
	s := trimReader{rd: io.LimitReader(r, maxSize)}
	tp := textproto.NewReader(bufio.NewReader(s))

	// Parse the main headers
	hdrs, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	e := Email{
		Subject: hdrs.Get(subject),
		To:      hdrs[to],
		CC:      hdrs[cc],
		BCC:     hdrs[bcc],
		From:    hdrs.Get(from),
		Headers: hdrs,
	}

	for _, hv := range [...]string{subject, to, cc, bcc} {
		delete(hdrs, hv)
	}

	// Recursively parse the MIME parts
	ps, err := parseMIMEParts(e.Headers, tp.R)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		switch p.ctyp {
		case "text/plain":
			e.Text = p.body
		case "text/html":
			e.HTML = p.body
		}
	}
	return &e, nil
}

// DefaultEmailSize is the largest email allowed to be read from NewFromReader.
const DefaultEmailSize = 1 << 20 // 1 MB

// New constructs an Email from an io.Reader. The data is expected to be in
// RFC 5322 format.
func New(r io.Reader) (*Email, error) {
	return NewWithSize(r, DefaultEmailSize)
}

// Close closes all of the Email's attachments.
func (e *Email) Close() error {
	for _, a := range e.Attachments {
		if err := a.Body.Close(); err != nil {
			return err
		}
	}
	return nil
}

// part is a copyable representation of a multipart.Part
type part struct {
	ctyp string
	body []byte
}

func parseMediaType(p textproto.MIMEHeader) (mtype, boundary string, err error) {
	if _, ok := p[contentType]; !ok {
		p.Set(contentType, defaultContentType)
	}
	mtyp, params, err := mime.ParseMediaType(p.Get(contentType))
	if err != nil {
		return "", "", err
	}
	return mtyp, params["boundary"], nil
}

func parseMultipart(r io.Reader, boundary string) (ps []part, err error) {
	if boundary == "" {
		return nil, ErrMissingBoundary
	}
	mr := multipart.NewReader(r, boundary)
	for {
		p, err := mr.NextPart()
		if err != nil {
			if err == io.EOF {
				return ps, nil
			}
			return nil, err
		}
		pp, err := parseMIMEParts(p.Header, p)
		if err != nil {
			return nil, err
		}
		ps = append(ps, pp...)
	}
}

func parseMIMEParts(p textproto.MIMEHeader, r io.Reader) (ps []part, err error) {
	mtyp, bdy, err := parseMediaType(p)
	if err != nil {
		return nil, err
	}

	if strings.HasPrefix(mtyp, "multipart/") {
		sps, err := parseMultipart(r, bdy)
		if err != nil {
			return nil, err
		}
		ps = append(ps, sps...)
	} else {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return nil, err
		}
		ps = []part{{body: buf.Bytes(), ctyp: mtyp}}
	}
	return ps, nil
}

// Attach attaches an io.ReadCloser to the email, using the provided name and
// content type. If contentType == "" and the io.ReadCloser implements
// io.Seeker, the content type will be sniffed. Otherwise,
// "application/octet-stream" will be used.
func (e *Email) Attach(rc io.ReadCloser, filename, contentType string) (err error) {
	if contentType == "" {
		if rs, ok := rc.(io.ReadSeeker); ok {
			if contentType, err = sniffType(filename, rs); err != nil {
				rc.Close()
				return err
			}
		} else {
			contentType = "application/octet-stream"
		}
	}

	e.Attachments = append(e.Attachments, Attachment{
		Name: filename,
		Header: textproto.MIMEHeader{
			contentDispo: []string{
				fmt.Sprintf(`attachment;\r\n filename="%s"`, filename),
			},
			contentID: []string{
				fmt.Sprintf("<%s>", filename),
			},
			contentXferEncoding: []string{"base64"},
			contentType:         []string{contentType},
		},
		Body: rc,
	})
	return nil
}

// AttachFile attaches a file from disk. Its content type is automatically
// detected.
func (e *Email) AttachFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	return e.Attach(file, filename, "")
}

func sniffType(name string, rs io.ReadSeeker) (string, error) {
	if ctype := mime.TypeByExtension(filepath.Ext(name)); ctype != "" {
		return ctype, nil
	}

	var buf [512]byte
	n, err := rs.Read(buf[:])
	if err != nil {
		return "", err
	}
	if _, err := rs.Seek(0, os.SEEK_SET); err != nil {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

// msgHeaders merges the Email's various fields and custom headers together in
// a standards-compliant way to create a MIMEHeader to be used in the resulting
// message. It does not alter e.Headers.
//
// "e"'s fields To, Cc, From, Subject will be used unless they are present in
// e.Headers. Unless set in e.Headers, date will filled with the current time.
func (e *Email) msgHeaders() (textproto.MIMEHeader, error) {
	res := make(textproto.MIMEHeader, len(e.Headers))
	if e.Headers != nil {
		for _, h := range [...]string{
			to, cc, from, subject, date, msgID,
		} {
			res[h] = e.Headers[h]
		}
	}

	if _, ok := res[to]; !ok && len(e.To) > 0 {
		res.Set(to, strings.Join(e.To, ", "))
	}
	if _, ok := res[cc]; !ok && len(e.CC) > 0 {
		res.Set(cc, strings.Join(e.CC, ", "))
	}
	if _, ok := res[subject]; !ok && e.Subject != "" {
		res.Set(subject, e.Subject)
	}
	// From, Return-Path, and Date are required headers.
	if _, ok := res[from]; !ok {
		if e.From == "" {
			return nil, errors.New("email: 'From' field cannot be empty")
		}
		res.Set(from, e.From)
	}

	now := time.Now()
	if _, ok := res[date]; !ok {
		res.Set(date, now.Format(time.RFC1123Z))
	}
	if _, ok := res[msgID]; !ok {
		res.Set(msgID, e.messageID(now))
	}
	if _, ok := res[mimeVers]; !ok {
		res.Set(mimeVers, "1.0")
	}
	for field, vals := range e.Headers {
		if _, ok := res[field]; !ok {
			res[field] = vals
		}
	}
	return res, nil
}

// MarshalText serializes the Email, making it ready to be sent on the write.
// In general, WriteTo should be preferred. It implements
// encoding.TextMarshaler.
func (e *Email) MarshalText() ([]byte, error) {
	const avgSize = 1024 * 75 // the internet says the average email is 75kB.
	var buf [avgSize]byte
	b := bytes.NewBuffer(buf[0:0])
	if _, err := e.WriteTo(b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// countWriter counts how many bytes are written to w.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) WriteString(p string) (n int, err error) {
	return io.WriteString(c.w, p)
}

// WriteTo writes a serialized Email to w. It implements io.WriterTo.
func (e *Email) WriteTo(w io.Writer) (int64, error) {
	cw := countWriter{w: w}
	err := e.writeTo(&cw)
	return cw.n, err
}

func (e *Email) writeTo(w io.Writer) error {
	hdrs, err := e.msgHeaders()
	if err != nil {
		return err
	}

	mw := multipart.NewWriter(w)

	// TODO: determine the content type based on message/attachment mix.
	hdrs.Set(
		contentType,
		fmt.Sprintf("multipart/mixed;\r\n boundary=%s", mw.Boundary()),
	)
	writeHeader(w, hdrs)
	io.WriteString(w, lineEnding)

	// Start the multipart/mixed part
	fmt.Fprintf(w, "--%s\r\n", mw.Boundary())
	header := make(textproto.MIMEHeader)

	// Check to see if there is a Text or HTML field
	if len(e.Text) > 0 || len(e.HTML) > 0 {
		sw := multipart.NewWriter(w)

		// Create the multipart alternative part
		header.Set(contentType,
			fmt.Sprintf(
				"multipart/alternative;\r\n boundary=%s\r\n",
				sw.Boundary(),
			),
		)
		// Write the header
		writeHeader(w, header)

		writeBody := func(content []byte, ctype string) error {
			if len(content) == 0 {
				return nil
			}
			header.Set(contentType, ctype)
			header.Set(contentXferEncoding, "quoted-printable")
			if _, err := sw.CreatePart(header); err != nil {
				return err
			}
			qp := quotedprintable.NewWriter(w)
			if _, err := qp.Write(content); err != nil {
				return err
			}
			return qp.Close()
		}

		writeBody(e.Text, "text/plain; charset=UTF-8")
		writeBody(e.HTML, "text/html; charset=UTF-8")

		if err := sw.Close(); err != nil {
			return err
		}
	}

	if len(e.Attachments) > 0 {
		var (
			cw  chunkWriter
			enc = base64.NewEncoder(base64.StdEncoding, &cw)
		)

		// Create attachment part, if necessary
		for _, a := range e.Attachments {
			part, err := mw.CreatePart(a.Header)
			if err != nil {
				return err
			}
			cw.w = part
			if _, err := io.Copy(enc, a.Body); err != nil {
				return err
			}
		}

		if err := enc.Close(); err != nil {
			return err
		}
		if err := cw.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}

var errClosed = errors.New("email: chunkWriter is closed")

// chunkWriter writes in blocks of MaxLineLength, ending each line with CRLF.
type chunkWriter struct {
	w   io.Writer
	n   int
	err error
}

func (c *chunkWriter) Write(p []byte) (n int, err error) {
	if c.err != nil {
		return 0, c.err
	}

	for len(p) != 0 {
		m := maxLineLength - c.n
		if m == 0 {
			m = maxLineLength
		}
		if m > len(p) {
			m = len(p)
		}

		m, err = c.w.Write(p[:m])
		n += m
		c.n += m
		if err != nil {
			return n, err
		}
		if c.n == maxLineLength {
			if _, err = io.WriteString(c.w, lineEnding); err != nil {
				return n, err
			}
			c.n = 0
		}
		p = p[m:]
	}
	return n, nil
}

func (c *chunkWriter) Close() error {
	if c.err != nil {
		return c.err
	}
	c.err = errClosed
	_, err := io.WriteString(c.w, lineEnding)
	return err
}

// Attachment represents an email attachment.
type Attachment struct {
	Name   string               // filename
	Header textproto.MIMEHeader // associated headers
	Body   io.ReadCloser        // attachment itself
}

// writeHeader writes the a header. If there are multiple values for a field,
// multiple "Field: value\r\n" lines will be emitted.
func writeHeader(w io.Writer, header textproto.MIMEHeader) {
	for field, vals := range header {
		for _, subval := range vals {
			io.WriteString(w, field)
			io.WriteString(w, ": ")
			switch field {
			case contentType, contentDispo:
				io.WriteString(w, subval)
			default:
				io.WriteString(w, mime.QEncoding.Encode("UTF-8", subval))
			}
			io.WriteString(w, lineEnding)
		}
	}
}

var hostname = func() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "localhost.localdomain"
	}
	return hostname
}()

func (e *Email) messageID(ts time.Time) string {
	h := sha256.New()

	// h.Write does not return errors.
	io.WriteString(h, e.From)
	io.WriteString(h, e.Subject)
	var buf [len(time.RFC3339)]byte
	ts.Round(5*time.Minute).AppendFormat(buf[0:0], time.RFC3339)
	h.Write(e.Text)
	h.Write(e.HTML)

	var hash [sha256.Size]byte
	return fmt.Sprintf("<%x@%s>", h.Sum(hash[0:0])[4:20], hostname)
}
