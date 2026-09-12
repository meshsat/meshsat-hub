// Package takenroll builds the enrolment data packages a TAK client imports to
// connect to a tenant's hosted TAK server: the connection settings, the tenant's
// CA as a Java truststore, and the phone's own client certificate.
//
// WHY THIS IS COPIED FROM UPSTREAM RATHER THAN DESIGNED. Nothing in this repo
// wrote a .pref, a PKCS#12 or a MissionPackageManifest before, and the formats
// are not documented anywhere we control -- they are whatever ATAK, iTAK and
// WinTAK happen to accept. So the shapes here are read out of the OpenTAKServer
// 1.7.13 wheel we already ship (`opentakserver/certificate_authority.py`,
// generate_zip), which is the server that feeds real phones today. Every
// deviation from it is deliberate and commented. Do not "tidy" these structures
// toward something that looks more sensible: the only authority is a client
// accepting the file.
//
// THE THREE SHAPES, none of which is guessable:
//
//   - ATAK/WinTAK take a data package NESTED INSIDE another data package.
//     The inner zip holds MANIFEST/manifest.xml plus a folder named by a
//     hardcoded uid containing preference.pref, the truststore and the client
//     p12. The outer zip holds its own MANIFEST plus a second hardcoded folder
//     containing the inner zip. Upstream's comment on the outer one is
//     "because WinTAK" -- so the nesting is a client compatibility workaround,
//     not structure for its own sake.
//   - iTAK takes a FLAT zip: config.pref, the truststore and the client p12 at
//     the root, with no MANIFEST at all.
//   - The two .pref files differ in more than layout: ATAK's references the
//     certificates by ABSOLUTE Android path, iTAK's by a path relative to a
//     cert/ directory that is not where the files actually sit in the zip.
//     Both are upstream's, verbatim.
//
// WHAT THIS PACKAGE DELIBERATELY LEAVES OUT of upstream's template: the five
// update-server preferences (appMgmtEnableUpdateServer, atakUpdateServerUrl,
// repoStartupSync, updateServerCaLocation, updateServerCaPassword) and
// deviceProfileEnableOnConnect. Our instances expose no public Marti or package
// repository -- the release gate asserts /Marti/api/tls and friends are refused
// publicly -- so shipping a phone an update-server URL would point it at
// something we intend to be unreachable, and enabling device profiles would turn
// on an OpenTAKServer feature nothing here has tested.
package takenroll

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"

	"software.sslmate.com/src/go-pkcs12"
)

// The folder names are upstream literals, not hashes of anything: generate_zip
// hardcodes both. Keeping them identical is the cheapest way to stay on the path
// real clients already accept.
const (
	innerFolder = "5c2bfcae3d98c9f4d262172df99ebac5"
	outerFolder = "80b828699e074a239066d454a76284eb"

	truststoreName = "truststore-root.p12"
)

// The PKCS#12 password. It is pkcs12.DefaultPassword ("changeit") on purpose and
// it is NOT a secret: ATAK cannot prompt for one during an automated import, so
// the password has to travel in the .pref beside the files it protects. What
// protects this bundle is that it is delivered once, through a single-use
// nonce-gated URL, and that the certificate inside it is useless without the
// tenant's CA having issued it. Choosing a random password here would put that
// random password in the same zip, one file away.
const p12Password = pkcs12.DefaultPassword

// usernamePattern is the operator's rule, restated rather than imported: a
// TakCertificateRequest whose username does not match is DENIED, because
// OpenTAKServer 1.7.13 rejects hyphenated usernames. Checking it here turns "the
// customer gets a package that silently cannot authenticate" into an error at
// the moment they ask for it. Keep in step with internal/takoperator.
var usernamePattern = regexp.MustCompile(`^[a-z0-9]{3,32}$`)

// Input is everything a package needs. The private key is generated in Hub
// memory for exactly one request and is never persisted: the Hub asks the
// operator to sign a CSR, builds the p12, hands it over once and forgets it.
// That is why this takes PEM bytes rather than a store handle.
type Input struct {
	Host        string // what the phone dials, e.g. hub.meshsat.net
	Port        int    // the TAK SSL streaming port, 8089
	Username    string // the OTS account; also the certificate's CN
	Description string // what the client shows in its server list
	KeyPEM      []byte // the phone's private key
	CertPEM     []byte // the leaf the operator issued, CN == Username
	// CACertPEM is the TENANT's CA: the chain the phone's own certificate was
	// issued under. It goes in the client p12 beside the leaf.
	CACertPEM []byte
	// ServerTrustPEM is the chain of the certificate THE FRONT PRESENTS, and it
	// is what the truststore carries.
	//
	// These are two different certificates and conflating them is the obvious
	// mistake -- it was made here once. The phone's certificate is issued by the
	// tenant's CA, which is how the front works out which tenant is calling. The
	// front's own certificate is a public one for the Hub's hostname (today a
	// Let's Encrypt `*.meshsat.net`), because ATAK sends no SNI on the CoT socket
	// so one certificate has to answer every tenant. `caLocation` in the .pref is
	// what the phone verifies THAT against, so a truststore holding the tenant CA
	// makes every handshake fail -- and it fails at the client, where we would
	// never see it.
	//
	// internal/config says the same thing about the front's certificate: "Every
	// tenant's truststore therefore carries this certificate's chain."
	ServerTrustPEM []byte
}

// Bundle is one file to hand to a customer.
type Bundle struct {
	Filename string
	Data     []byte
}

func (in Input) validate() error {
	switch {
	case in.Host == "":
		return fmt.Errorf("takenroll: no host")
	case in.Port <= 0 || in.Port > 65535:
		return fmt.Errorf("takenroll: port %d is not a port", in.Port)
	case !usernamePattern.MatchString(in.Username):
		// Named precisely: this is the rule that would otherwise fail later, in
		// the operator, as a CertDenied the customer cannot act on.
		return fmt.Errorf("takenroll: username %q must match %s "+
			"(OpenTAKServer rejects hyphens, dots and @)", in.Username, usernamePattern)
	case len(in.KeyPEM) == 0, len(in.CertPEM) == 0, len(in.CACertPEM) == 0:
		return fmt.Errorf("takenroll: key, certificate and CA are all required")
	case len(in.ServerTrustPEM) == 0:
		// Refused rather than defaulted to the tenant CA. A package whose
		// truststore cannot verify the front is one the phone rejects at the
		// handshake, which looks like "TAK does not work" and is invisible here.
		return fmt.Errorf("takenroll: no server trust chain; the phone would have " +
			"nothing to verify the TAK front's certificate against")
	}
	return nil
}

// Build returns the ATAK/WinTAK package and the iTAK package. Both are returned
// together because a tenant does not know which client its people run, and
// building one without the other just moves the question to support.
func Build(in Input) (atak, itak Bundle, err error) {
	if err := in.validate(); err != nil {
		return Bundle{}, Bundle{}, err
	}

	clientP12, trustP12, err := in.keystores()
	if err != nil {
		return Bundle{}, Bundle{}, err
	}
	userFile := in.Username + ".p12"

	atakData, err := in.atakPackage(clientP12, trustP12, userFile)
	if err != nil {
		return Bundle{}, Bundle{}, fmt.Errorf("takenroll: atak package: %w", err)
	}
	itakData, err := in.itakPackage(clientP12, trustP12, userFile)
	if err != nil {
		return Bundle{}, Bundle{}, fmt.Errorf("takenroll: itak package: %w", err)
	}
	return Bundle{Filename: in.Username + "_CONFIG.zip", Data: atakData},
		Bundle{Filename: in.Username + "_CONFIG_iTAK.zip", Data: itakData},
		nil
}

// keystores turns PEM into the two PKCS#12 files a client wants.
func (in Input) keystores() (client, trust []byte, err error) {
	key, err := parseKey(in.KeyPEM)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := parseCert(in.CertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("takenroll: certificate: %w", err)
	}
	cas, err := parseCerts(in.CACertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("takenroll: CA: %w", err)
	}
	if leaf.Subject.CommonName != in.Username {
		// The operator forces CN = username, so a mismatch means these two
		// arguments came from different requests. A package built from them
		// would be refused by the server's handle_auth, which looks the common
		// name up as an OTS user.
		return nil, nil, fmt.Errorf("takenroll: certificate CN %q is not the username %q",
			leaf.Subject.CommonName, in.Username)
	}

	// pkcs12.Legacy (= LegacyDES) and NOT Modern2023, which the package's own
	// documentation says needs Java 12 or higher. ATAK is Android Java on a long
	// tail of devices, and upstream OpenTAKServer shells out to
	// `openssl pkcs12 -legacy -export` for both of these files -- so the server
	// that actually feeds real phones agrees. A Modern2023 file would pass every
	// test in this repo and be refused by a handset.
	//
	// Note these are the Encoder METHODS. The package-level pkcs12.Encode and
	// pkcs12.EncodeTrustStore are LegacyRC2, are both marked Deprecated, and
	// LegacyRC2's own doc says OpenSSL 3 cannot read what it writes -- the two
	// most convenient names in the package are the worst choices in it.
	client, err = pkcs12.Legacy.Encode(key, leaf, cas, p12Password)
	if err != nil {
		return nil, nil, fmt.Errorf("takenroll: client p12: %w", err)
	}

	// The truststore is the FRONT's chain, not the tenant's. See Input.
	anchors, err := trustAnchors(in.ServerTrustPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("takenroll: server trust chain: %w", err)
	}
	// EncodeTrustStore marks the certificates with the OID that makes this a
	// Java 1.8+ TrustStore, which is what the client wants for caLocation.
	trust, err = pkcs12.Legacy.EncodeTrustStore(anchors, p12Password)
	if err != nil {
		return nil, nil, fmt.Errorf("takenroll: truststore: %w", err)
	}
	return client, trust, nil
}

// trustAnchors picks the certificates a client should trust out of a served
// chain.
//
// A Kubernetes tls.crt is leaf-first: leaf, then intermediate, then sometimes the
// root. The leaf is not a trust anchor -- pinning it would break the phone the
// next time the certificate is renewed, which for Let's Encrypt is every 60 days.
// So anchors are selected by IsCA rather than by position, which also handles a
// bare root, an intermediate-and-root pair, and a single self-signed certificate
// (a test or a private deployment) without any special cases.
func trustAnchors(pemBytes []byte) ([]*x509.Certificate, error) {
	certs, err := parseCerts(pemBytes)
	if err != nil {
		return nil, err
	}
	var cas []*x509.Certificate
	for _, c := range certs {
		if c.IsCA {
			cas = append(cas, c)
		}
	}
	if len(cas) > 0 {
		return cas, nil
	}
	// Nothing in the chain says it is a CA. Use what was given rather than
	// refusing: a deployment may hand us a single self-signed server certificate,
	// and trusting exactly that is a coherent choice the caller has made.
	return certs, nil
}

// xmlHeader is upstream's literal header, single quotes and standalone and all,
// rather than Go's xml.Header. The clients parse what they have always been
// given; this is not the place to modernise punctuation.
const xmlHeader = "<?xml version='1.0' standalone='yes'?>\n"

type prefEntry struct {
	XMLName xml.Name `xml:"entry"`
	Key     string   `xml:"key,attr"`
	Class   string   `xml:"class,attr"`
	Value   string   `xml:",chardata"`
}

type prefGroup struct {
	XMLName xml.Name    `xml:"preference"`
	Version string      `xml:"version,attr"`
	Name    string      `xml:"name,attr"`
	Entries []prefEntry `xml:"entry"`
}

type prefDoc struct {
	XMLName xml.Name    `xml:"preferences"`
	Groups  []prefGroup `xml:"preference"`
}

func str(key, value string) prefEntry {
	return prefEntry{Key: key, Class: "class java.lang.String", Value: value}
}

func boolean(key string, value bool) prefEntry {
	return prefEntry{Key: key, Class: "class java.lang.Boolean", Value: fmt.Sprintf("%t", value)}
}

func integer(key string, value int) prefEntry {
	return prefEntry{Key: key, Class: "class java.lang.Integer", Value: fmt.Sprintf("%d", value)}
}

// connectString is the one line that decides whether a phone reaches us at all:
// host:port:ssl, and the port is the public TAK port the edge listens on.
func (in Input) connectString() string {
	return fmt.Sprintf("%s:%d:ssl", in.Host, in.Port)
}

func (in Input) description() string {
	if in.Description != "" {
		return in.Description
	}
	return "MeshSat Hub"
}

// cotStreams is identical in both clients' files.
func (in Input) cotStreams() prefGroup {
	return prefGroup{Version: "1", Name: "cot_streams", Entries: []prefEntry{
		integer("count", 1),
		str("description0", in.description()),
		boolean("enabled0", true),
		str("connectString0", in.connectString()),
	}}
}

func (in Input) pref(caLocation, certLocation string) ([]byte, error) {
	doc := prefDoc{Groups: []prefGroup{
		in.cotStreams(),
		{Version: "1", Name: "com.atakmap.app_preferences", Entries: []prefEntry{
			boolean("displayServerConnectionWidget", true),
			str("caLocation", caLocation),
			str("caPassword", p12Password),
			str("clientPassword", p12Password),
			str("certificateLocation", certLocation),
		}},
	}}
	// Marshalled rather than templated so that a host name or description
	// carrying & or < cannot produce a file the client silently fails to parse.
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xmlHeader), append(body, '\n')...), nil
}

type manifestParam struct {
	XMLName xml.Name `xml:"Parameter"`
	Name    string   `xml:"name,attr"`
	Value   string   `xml:"value,attr"`
}

type manifestContent struct {
	XMLName  xml.Name `xml:"Content"`
	Ignore   string   `xml:"ignore,attr"`
	ZipEntry string   `xml:"zipEntry,attr"`
}

type manifestDoc struct {
	XMLName       xml.Name          `xml:"MissionPackageManifest"`
	Version       string            `xml:"version,attr"`
	Configuration []manifestParam   `xml:"Configuration>Parameter"`
	Contents      []manifestContent `xml:"Contents>Content"`
}

func manifest(uid, name string, onReceiveDelete bool, entries []string) ([]byte, error) {
	params := []manifestParam{{Name: "uid", Value: uid}, {Name: "name", Value: name}}
	if onReceiveDelete {
		params = append(params, manifestParam{Name: "onReceiveDelete", Value: "true"})
	}
	contents := make([]manifestContent, 0, len(entries))
	for _, e := range entries {
		contents = append(contents, manifestContent{Ignore: "false", ZipEntry: e})
	}
	body, err := xml.MarshalIndent(manifestDoc{
		Version:       "2",
		Configuration: params,
		Contents:      contents,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// uid returns a fresh package uid. ATAK keys an imported package by it, so two
// enrolments must not share one or the second can be taken for a re-import of
// the first.
func uid() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("takenroll: no randomness for a package uid: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func (in Input) atakPackage(clientP12, trustP12 []byte, userFile string) ([]byte, error) {
	pref, err := in.pref(
		// Absolute Android paths, as upstream's ATAK template has them.
		"/storage/emulated/0/atak/cert/"+truststoreName,
		"/storage/emulated/0/atak/cert/"+userFile,
	)
	if err != nil {
		return nil, err
	}
	innerUID, err := uid()
	if err != nil {
		return nil, err
	}
	name := in.description() + " " + in.Username
	innerManifest, err := manifest(innerUID, name, true, []string{
		innerFolder + "/preference.pref",
		innerFolder + "/" + truststoreName,
		innerFolder + "/" + userFile,
	})
	if err != nil {
		return nil, err
	}
	inner, err := zipOf(map[string][]byte{
		innerFolder + "/preference.pref":   pref,
		innerFolder + "/" + truststoreName: trustP12,
		innerFolder + "/" + userFile:       clientP12,
		"MANIFEST/manifest.xml":            innerManifest,
	})
	if err != nil {
		return nil, err
	}

	// The outer package exists only because WinTAK wants it (upstream's own
	// reason). It carries no certificates of its own -- just the inner package.
	outerUID, err := uid()
	if err != nil {
		return nil, err
	}
	innerName := in.Username + ".zip"
	outerManifest, err := manifest(outerUID, name+" CONFIG", false, []string{
		outerFolder + "/" + innerName,
	})
	if err != nil {
		return nil, err
	}
	return zipOf(map[string][]byte{
		outerFolder + "/" + innerName: inner,
		"MANIFEST/manifest.xml":       outerManifest,
	})
}

func (in Input) itakPackage(clientP12, trustP12 []byte, userFile string) ([]byte, error) {
	// iTAK's paths are relative to a cert/ directory even though the files sit
	// at the root of the zip. That is upstream's arrangement and iTAK's own
	// import puts them where it expects them; it is not a bug to "fix" here.
	pref, err := in.pref("cert/"+truststoreName, "cert/"+userFile)
	if err != nil {
		return nil, err
	}
	return zipOf(map[string][]byte{
		"config.pref":  pref,
		truststoreName: trustP12,
		userFile:       clientP12,
	})
}

// zipOf writes a deflated zip with forward-slash names and no filesystem
// metadata, so the same input always produces the same archive apart from the
// uids. Names are sorted for the same reason.
func zipOf(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sortStrings(names)

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, n := range names {
		f, err := w.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate})
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func parseKey(pemBytes []byte) (any, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, fmt.Errorf("takenroll: private key is not PEM")
	}
	// Accept all three encodings rather than assuming one: the operator issues
	// RSA today (that ecosystem is RSA by convention) but nothing here needs to
	// care, and a mismatch would otherwise surface as an opaque p12 error.
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("takenroll: private key is PEM but not PKCS#8, PKCS#1 or SEC 1")
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	certs, err := parseCerts(pemBytes)
	if err != nil {
		return nil, err
	}
	if len(certs) != 1 {
		return nil, fmt.Errorf("takenroll: expected one certificate, got %d", len(certs))
	}
	return certs[0], nil
}

func parseCerts(pemBytes []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := pemBytes
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("takenroll: no CERTIFICATE block in %d bytes of PEM",
			len(bytes.TrimSpace(pemBytes)))
	}
	return out, nil
}

// Describe is for the audit log and the UI: what was handed over, without any of
// it. Never log a bundle's bytes -- it carries a private key.
func (b Bundle) Describe() string {
	return fmt.Sprintf("%s (%d bytes)", b.Filename, len(b.Data))
}

// ClientNames is what the UI offers to download, in the order it should show
// them: most people have Android.
func ClientNames() []string { return []string{"ATAK / WinTAK", "iTAK"} }

// SanitiseUsername turns a person's identifier into one OpenTAKServer accepts,
// or returns false when nothing usable is left. Exported because the UI should
// show the customer what their account will actually be called BEFORE the
// operator denies it.
func SanitiseUsername(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	return out, usernamePattern.MatchString(out)
}
