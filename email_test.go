package email

import (
	"encoding"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	"bytes"
	"crypto/rand"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
)

var (
	_ interface {
		encoding.TextMarshaler
		io.WriterTo
	} = (*Email)(nil)
	_ io.WriteCloser = (*chunkWriter)(nil)
)

var dummyEmail = Email{
	From:    "John Smith <test@gmail.com>",
	To:      []string{"test@example.com"},
	BCC:     []string{"test_bcc@example.com", "test2_bcc@example.com"},
	CC:      []string{"test_cc@example.com"},
	Subject: "Awesome Subject",
	Text:    []byte("Text Body is, of course, supported!\n"),
	HTML:    []byte("<h1>Fancy Html is supported, too!</h1>\n"),
}

func TestEmail_Attach(t *testing.T) {
	e := dummyEmail
	e.Attach(
		ioutil.NopCloser(bytes.NewBufferString("awesome attachement")),
		"rad.txt",
		"text/plain; charset=utf-8",
	)

	pr, pw := io.Pipe()

	go func() {
		if _, err := e.WriteTo(pw); err != nil {
			panic(err)
		}
		// if err := pw.Close(); err != nil {
		//	panic(err)
		// }
	}()

	msg, err := mail.ReadMessage(pr)
	if err != nil {
		t.Fatal("could not parse rendered message: ", err)
	}

	// ReadMessage just reads the header.
	// if err := pr.Close(); err != nil {
	//	t.Fatal(err)
	// }

	expectedHeaders := map[string][]string{
		"To":      e.To,
		"From":    []string{e.From},
		"Cc":      e.CC,
		"Subject": []string{e.Subject},
	}

	for header, expected := range expectedHeaders {
		got := msg.Header[header]
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("wrong value for message header %s: %v != %v",
				header, expected, got)
		}
	}

	// Were the right headers set?
	ct := msg.Header.Get(contentType)
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal("Content-Type header is invalid: ", ct)
	} else if mt != "multipart/mixed" {
		t.Fatalf("Content-Type expected \"multipart/mixed\", not %v", mt)
	}
	b := params["boundary"]
	if b == "" {
		t.Fatalf("Invalid or missing boundary parameter: ", b)
	}
	if len(params) != 1 {
		t.Fatal("Unexpected content-type parameters")
	}

	// Is the generated message parsable?
	mixed := multipart.NewReader(msg.Body, params["boundary"])

	text, err := mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find text component of email: %v", err)
	}

	// Does the text portion match what we expect?
	mt, params, err = mime.ParseMediaType(text.Header.Get(contentType))
	if err != nil {
		t.Fatal("Could not parse message's Content-Type")
	} else if mt != "multipart/alternative" {
		t.Fatal("Message missing multipart/alternative")
	}
	mpReader := multipart.NewReader(text, params["boundary"])
	part, err := mpReader.NextPart()
	if err != nil {
		t.Fatal("Could not read plain text component of message: ", err)
	}
	plainText, err := ioutil.ReadAll(part)
	if err != nil {
		t.Fatal("Could not read plain text component of message: ", err)
	}
	if !bytes.Equal(plainText, []byte("Text Body is, of course, supported!\r\n")) {
		t.Fatalf("Plain text is broken: %#q", plainText)
	}

	// Check attachments.
	_, err = mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find attachemnt compoenent of email: ", err)
	}

	if _, err = mixed.NextPart(); err != io.EOF {
		t.Error("Expected only text and one attachement!")
	}

}

func TestNew(t *testing.T) {
	ex := &Email{
		Subject: "Test Subject",
		To:      []string{"John Smith <jsmith@gmail.com>"},
		From:    "John Smith <jsmith@gmail.com>",
		Text:    []byte("This is a test email with HTML Formatting. It also has very long lines so\nthat the content must be wrapped if using quoted-printable decoding.\n"),
		HTML:    []byte("<div dir=\"ltr\">This is a test email with <b>HTML Formatting.</b>\u00a0It also has very long lines so that the content must be wrapped if using quoted-printable decoding.</div>\n"),
	}
	const raw = `MIME-Version: 1.0
Subject: Test Subject
From: John Smith <jsmith@gmail.com>
To: John Smith <jsmith@gmail.com>
Content-Type: multipart/alternative; boundary=001a114fb3fc42fd6b051f834280

--001a114fb3fc42fd6b051f834280
Content-Type: text/plain; charset=UTF-8

This is a test email with HTML Formatting. It also has very long lines so
that the content must be wrapped if using quoted-printable decoding.

--001a114fb3fc42fd6b051f834280
Content-Type: text/html; charset=UTF-8
Content-Transfer-Encoding: quoted-printable

<div dir=3D"ltr">This is a test email with <b>HTML Formatting.</b>=C2=A0It =
also has very long lines so that the content must be wrapped if using quote=
d-printable decoding.</div>

--001a114fb3fc42fd6b051f834280--`

	e, err := New(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("error creating email %v", err)
	}
	if e.Subject != ex.Subject {
		t.Fatalf(`incorrect subject:
want: %q
got : %q
`, e.Subject, ex.Subject)
	}
	if !bytes.Equal(e.Text, ex.Text) {
		t.Fatalf(`incorrect text:
want: %q
got : %q
`, e.Text, ex.Text)
	}
	if !bytes.Equal(e.HTML, ex.HTML) {
		t.Fatalf(`incorrect HTML:
want: %q
got : %q
`, e.HTML, ex.HTML)
	}
	if e.From != ex.From {
		t.Fatalf(`incorrect "From":
want: %q
got : %q
`, e.From, ex.From)
	}

}

func TestNonMultipartEmailFromReader(t *testing.T) {
	ex := &Email{
		Text:    []byte("This is a test message!"),
		Subject: "Example Subject (no MIME Type)",
		Headers: textproto.MIMEHeader{},
	}
	ex.Headers.Add(contentType, "text/plain; charset=us-ascii")
	ex.Headers.Add("Message-ID", "<foobar@example.com>")
	raw := []byte(`From: "Foo Bar" <foobar@example.com>
Content-Type: text/plain
To: foobar@example.com 
Subject: Example Subject (no MIME Type)
Message-ID: <foobar@example.com>

This is a test message!`)
	e, err := New(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error creating email %s", err.Error())
	}
	if ex.Subject != e.Subject {
		t.Errorf("Incorrect subject. %#q != %#q\n", ex.Subject, e.Subject)
	}
	if !bytes.Equal(ex.Text, e.Text) {
		t.Errorf("Incorrect body. %#q != %#q\n", ex.Text, e.Text)
	}
	if ex.Headers.Get("Message-ID") != e.Headers.Get("Message-ID") {
		t.Errorf("Incorrect message ID. %#q != %#q\n", ex.Headers.Get("Message-ID"), e.Headers.Get("Message-ID"))
	}
}

func Example_WriteTo() {
	e := Email{
		From:    "John Smith <test@gmail.com>",
		To:      []string{"test@example.com"},
		BCC:     []string{"test_bcc@example.com"},
		CC:      []string{"test_cc@example.com"},
		Subject: "Awesome Subject",
		Text:    []byte("Text Body is, of course, supported!\n"),
		HTML:    []byte("<h1>Fancy Html is supported, too!</h1>\n"),
	}

	var w io.Writer // network socket, etc.

	if _, err := e.WriteTo(w); err != nil {
		// handle error...
	}
}

func ExampleAttach() {
	var e Email
	e.AttachFile("test.txt")
}

func Test_chunkWriter(t *testing.T) {
	const (
		file = "I'm a file long enough to force the function to wrap a\n" +
			"couple of lines, but I stop short of the end of one line and\n" +
			"have some padding dangling at the end."
		encoded = "SSdtIGEgZmlsZSBsb25nIGVub3VnaCB0byBmb3JjZSB0aGUgZnVuY3Rpb24gdG8gd3JhcCBhCmNv\r\n" +
			"dXBsZSBvZiBsaW5lcywgYnV0IEkgc3RvcCBzaG9ydCBvZiB0aGUgZW5kIG9mIG9uZSBsaW5lIGFu\r\n" +
			"ZApoYXZlIHNvbWUgcGFkZGluZyBkYW5nbGluZyBhdCB0aGUgZW5kLg==\r\n"
	)

	var (
		buf bytes.Buffer
		cw  = &chunkWriter{w: &buf}
		dst = base64.NewEncoder(base64.StdEncoding, cw)
		src = strings.NewReader(file)
	)
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	if bs := buf.String(); bs != encoded {
		t.Fatalf(`encoded file does not match expected:
want: %q
got : %q
`, encoded, bs)
	}
}

// *Since the mime library in use by ```email``` is now in the stdlib, this test is deprecated
func Test_quotedPrintEncode(t *testing.T) {
	var buf bytes.Buffer
	text := []byte("Dear reader!\n\n" +
		"This is a test email to try and capture some of the corner cases that exist within\n" +
		"the quoted-printable encoding.\n" +
		"There are some wacky parts like =, and this input assumes UNIX line breaks so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's a fish: 🐟\n")
	expected := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist=\r\n" +
		" within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =3D, and this input assumes UNIX line break=\r\n" +
		"s so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's=\r\n" +
		" a fish: =F0=9F=90=9F\r\n")
	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write(text); err != nil {
		t.Fatal("quotePrintEncode: ", err)
	}
	if err := qp.Close(); err != nil {
		t.Fatal("Error closing writer", err)
	}
	if b := buf.Bytes(); !bytes.Equal(b, expected) {
		t.Errorf("quotedPrintEncode generated incorrect results: %#q != %#q", b, expected)
	}
}

func TestMultipartNoContentType(t *testing.T) {
	raw := []byte(`From: Mikhail Gusarov <dottedmag@dottedmag.net>
To: notmuch@notmuchmail.org
References: <20091117190054.GU3165@dottiness.seas.harvard.edu>
Date: Wed, 18 Nov 2009 01:02:38 +0600
Message-ID: <87iqd9rn3l.fsf@vertex.dottedmag>
MIME-Version: 1.0
Subject: Re: [notmuch] Working with Maildir storage?
Content-Type: multipart/mixed; boundary="===============1958295626=="

--===============1958295626==
Content-Type: multipart/signed; boundary="=-=-=";
    micalg=pgp-sha1; protocol="application/pgp-signature"

--=-=-=
Content-Transfer-Encoding: quoted-printable

Twas brillig at 14:00:54 17.11.2009 UTC-05 when lars@seas.harvard.edu did g=
yre and gimble:

--=-=-=
Content-Type: application/pgp-signature

-----BEGIN PGP SIGNATURE-----
Version: GnuPG v1.4.9 (GNU/Linux)

iQIcBAEBAgAGBQJLAvNOAAoJEJ0g9lA+M4iIjLYQAKp0PXEgl3JMOEBisH52AsIK
=/ksP
-----END PGP SIGNATURE-----
--=-=-=--

--===============1958295626==
Content-Type: text/plain; charset="us-ascii"
MIME-Version: 1.0
Content-Transfer-Encoding: 7bit
Content-Disposition: inline

Testing!
--===============1958295626==--
`)
	e, err := New(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error when parsing email %s", err.Error())
	}
	if !bytes.Equal(e.Text, []byte("Testing!")) {
		t.Fatalf("Error incorrect text: %#q != %#q\n", e.Text, "Testing!")
	}
}

// *Since the mime library in use by ```email``` is now in the stdlib, this test is deprecated
func Test_quotedPrintDecode(t *testing.T) {
	text := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist=\r\n" +
		" within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =3D, and this input assumes UNIX line break=\r\n" +
		"s so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's=\r\n" +
		" a fish: =F0=9F=90=9F\r\n")
	expected := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =, and this input assumes UNIX line breaks so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's a fish: 🐟\r\n")
	qp := quotedprintable.NewReader(bytes.NewReader(text))
	got, err := ioutil.ReadAll(qp)
	if err != nil {
		t.Fatal("quotePrintDecode: ", err)
	}

	if !bytes.Equal(got, expected) {
		t.Errorf("quotedPrintDecode generated incorrect results: %#q != %#q", got, expected)
	}
}

var gerr error

func Benchmark_chunkWriter(b *testing.B) {
	b.StopTimer()
	var buf [1 << 17]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	var (
		dst  = &chunkWriter{w: ioutil.Discard}
		src  = bytes.NewReader(buf[:])
		lerr error
	)
	b.StartTimer()

	for i := 0; i < b.N; i++ {
		if _, lerr = io.Copy(dst, src); lerr != nil {
			panic(lerr)
		}
	}
	gerr = lerr
}

var gid string

func BenchmarkEmail_messageID(b *testing.B) {
	var (
		lid string
		now = time.Now()
	)
	for i := 0; i < b.N; i++ {
		lid = dummyEmail.messageID(now)
	}
	gid = lid
}
