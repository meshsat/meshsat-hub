package takenroll

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"encoding/xml"
	"io"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// The enrolment package is a file format contract with software we do not own,
// so these tests decode what was produced rather than checking that the builder
// called the functions it was told to. Anything asserted here is something a
// phone would otherwise have to tell us, slowly.

type fixture struct {
	in      Input
	caCert  *x509.Certificate
	leafKey *rsa.PrivateKey
	atak    Bundle
	itak    Bundle
}

// The front's certificate chain, which is NOT the tenant CA. Generated once: it
// is identical for every test and two RSA keys per test is pure cost.
//
// Shaped like a real tls.crt -- leaf first, then the issuing CA -- so the tests
// exercise the same ordering Kubernetes hands us.
var (
	frontOnce  sync.Once
	frontChain []byte
	frontCA    *x509.Certificate
	frontLeaf  *x509.Certificate
)

func frontTrust(t *testing.T) ([]byte, *x509.Certificate, *x509.Certificate) {
	t.Helper()
	frontOnce.Do(func() {
		caKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		caTmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(100),
			Subject:               pkix.Name{CommonName: "Test Front Issuing CA"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign,
		}
		caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		frontCA, err = x509.ParseCertificate(caDER)
		if err != nil {
			panic(err)
		}
		leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(101),
			Subject:      pkix.Name{CommonName: "*.meshsat.net"},
			DNSNames:     []string{"*.meshsat.net", "meshsat.net"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}, frontCA, &leafKey.PublicKey, caKey)
		if err != nil {
			panic(err)
		}
		frontLeaf, err = x509.ParseCertificate(leafDER)
		if err != nil {
			panic(err)
		}
		frontChain = append(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
		)
	})
	return frontChain, frontCA, frontLeaf
}

func build(t *testing.T, mutate func(*Input)) fixture {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tenant abcd123456 CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "phone01"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	chain, _, _ := frontTrust(t)
	in := Input{
		Host:     "hub.meshsat.net",
		Port:     8089,
		Username: "phone01",
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)}),
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		// The TENANT's CA: the chain the phone's own certificate belongs to.
		CACertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		// The FRONT's chain: what the truststore must carry instead.
		ServerTrustPEM: chain,
	}
	if mutate != nil {
		mutate(&in)
	}
	atak, itak, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return fixture{in: in, caCert: caCert, leafKey: leafKey, atak: atak, itak: itak}
}

func unzip(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// TestATAKPackageIsNestedAsWinTAKRequires is the structural assertion. Upstream
// wraps the real package in a second one "because WinTAK"; flattening it would
// look tidier and would stop WinTAK importing it.
func TestATAKPackageIsNestedAsWinTAKRequires(t *testing.T) {
	f := build(t, nil)

	outer := unzip(t, f.atak.Data)
	want := []string{"80b828699e074a239066d454a76284eb/phone01.zip", "MANIFEST/manifest.xml"}
	if got := keys(outer); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("outer package members = %v, want %v", got, want)
	}
	// The outer package must carry NO key material of its own.
	for name := range outer {
		if strings.HasSuffix(name, ".p12") {
			t.Errorf("outer package carries %s; certificates belong in the inner one", name)
		}
	}

	inner := unzip(t, outer["80b828699e074a239066d454a76284eb/phone01.zip"])
	wantInner := []string{
		"5c2bfcae3d98c9f4d262172df99ebac5/phone01.p12",
		"5c2bfcae3d98c9f4d262172df99ebac5/preference.pref",
		"5c2bfcae3d98c9f4d262172df99ebac5/truststore-root.p12",
		"MANIFEST/manifest.xml",
	}
	if got := keys(inner); strings.Join(got, ",") != strings.Join(wantInner, ",") {
		t.Fatalf("inner package members = %v, want %v", got, wantInner)
	}

	// Every Content the manifest promises must actually be in the zip: ATAK
	// refuses a package whose manifest lists a file it cannot find, and that is
	// a silent "nothing happened" on the handset.
	var man manifestDoc
	if err := xml.Unmarshal(inner["MANIFEST/manifest.xml"], &man); err != nil {
		t.Fatalf("inner manifest does not parse: %v", err)
	}
	if len(man.Contents) != 3 {
		t.Errorf("inner manifest lists %d contents, want 3", len(man.Contents))
	}
	for _, c := range man.Contents {
		if _, ok := inner[c.ZipEntry]; !ok {
			t.Errorf("manifest promises %q which is not in the zip", c.ZipEntry)
		}
	}
	if man.Version != "2" {
		t.Errorf("manifest version = %q, want 2", man.Version)
	}
}

func TestITAKPackageIsFlatAndHasNoManifest(t *testing.T) {
	f := build(t, nil)
	got := keys(unzip(t, f.itak.Data))
	want := []string{"config.pref", "phone01.p12", "truststore-root.p12"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("iTAK package members = %v, want %v", got, want)
	}
}

func innerPref(t *testing.T, f fixture) string {
	t.Helper()
	outer := unzip(t, f.atak.Data)
	inner := unzip(t, outer["80b828699e074a239066d454a76284eb/phone01.zip"])
	return string(inner["5c2bfcae3d98c9f4d262172df99ebac5/preference.pref"])
}

// TestTheConnectStringIsTheOneThingThatMustBeRight -- a phone with a wrong
// connectString does not fail loudly, it simply never appears.
func TestTheConnectStringIsTheOneThingThatMustBeRight(t *testing.T) {
	f := build(t, nil)
	for name, pref := range map[string]string{
		"atak": innerPref(t, f),
		"itak": string(unzip(t, f.itak.Data)["config.pref"]),
	} {
		if !strings.Contains(pref, "hub.meshsat.net:8089:ssl") {
			t.Errorf("%s pref has no host:port:ssl connect string:\n%s", name, pref)
		}
		var doc prefDoc
		if err := xml.Unmarshal([]byte(pref), &doc); err != nil {
			t.Fatalf("%s pref does not parse: %v", name, err)
		}
		if len(doc.Groups) != 2 {
			t.Errorf("%s pref has %d preference groups, want 2", name, len(doc.Groups))
		}
		if doc.Groups[0].Name != "cot_streams" {
			t.Errorf("%s first group = %q, want cot_streams", name, doc.Groups[0].Name)
		}
		// The class attributes are what make these Java types rather than
		// strings; ATAK reads them.
		for _, e := range doc.Groups[0].Entries {
			if e.Key == "count" && e.Class != "class java.lang.Integer" {
				t.Errorf("%s count class = %q", name, e.Class)
			}
			if e.Key == "enabled0" && e.Class != "class java.lang.Boolean" {
				t.Errorf("%s enabled0 class = %q", name, e.Class)
			}
		}
	}
}

// TestTheTwoClientsGetTheirOwnCertificatePaths: ATAK's are absolute Android
// paths and iTAK's are relative. They are not interchangeable and upstream uses
// both, so a single shared template would break one of the two.
func TestTheTwoClientsGetTheirOwnCertificatePaths(t *testing.T) {
	f := build(t, nil)
	atak := innerPref(t, f)
	if !strings.Contains(atak, "/storage/emulated/0/atak/cert/truststore-root.p12") {
		t.Errorf("ATAK pref lacks the absolute truststore path:\n%s", atak)
	}
	if !strings.Contains(atak, "/storage/emulated/0/atak/cert/phone01.p12") {
		t.Errorf("ATAK pref lacks the absolute client path:\n%s", atak)
	}
	itak := string(unzip(t, f.itak.Data)["config.pref"])
	if !strings.Contains(itak, ">cert/truststore-root.p12<") {
		t.Errorf("iTAK pref lacks the relative truststore path:\n%s", itak)
	}
	if strings.Contains(itak, "/storage/emulated") {
		t.Errorf("iTAK pref carries Android absolute paths:\n%s", itak)
	}
}

// TestTheKeystoresDecodeAsAClientWouldReadThem.
func TestTheKeystoresDecodeAsAClientWouldReadThem(t *testing.T) {
	f := build(t, nil)
	inner := unzip(t, unzip(t, f.atak.Data)["80b828699e074a239066d454a76284eb/phone01.zip"])

	key, leaf, cas, err := pkcs12.DecodeChain(inner["5c2bfcae3d98c9f4d262172df99ebac5/phone01.p12"], pkcs12.DefaultPassword)
	if err != nil {
		t.Fatalf("client p12 does not decode with the password in the pref: %v", err)
	}
	if leaf.Subject.CommonName != "phone01" {
		t.Errorf("client cert CN = %q, want phone01", leaf.Subject.CommonName)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("client p12 key is %T, want *rsa.PrivateKey", key)
	}
	if !rsaKey.PublicKey.Equal(f.leafKey.Public()) {
		t.Error("the p12 carries a different key than the certificate was issued for")
	}
	if len(cas) != 1 || !cas[0].Equal(f.caCert) {
		t.Errorf("client p12 carries %d CA certs, want the tenant CA", len(cas))
	}

	trust, err := pkcs12.DecodeTrustStore(inner["5c2bfcae3d98c9f4d262172df99ebac5/truststore-root.p12"], pkcs12.DefaultPassword)
	if err != nil {
		t.Fatalf("truststore does not decode: %v", err)
	}
	// The FRONT's issuer, not the tenant CA. See TestTheTruststoreCarriesTheFrontsChain.
	_, serverCA, _ := frontTrust(t)
	if len(trust) != 1 || !trust[0].Equal(serverCA) {
		t.Errorf("truststore holds %d certs, want the front's issuing CA", len(trust))
	}
}

// TestTheTruststoreCarriesTheFrontsChainAndNotTheTenantCA is the regression test
// for a defect this file's own test used to assert INTO existence: it checked
// that the truststore held the tenant CA, which is the wrong certificate.
//
// Two chains are in play. The phone's certificate is issued by the TENANT's CA,
// and the front verifies the phone against it -- that is how it knows which
// tenant is calling. The front's own certificate is a public one for the Hub's
// hostname, because ATAK sends no SNI on the CoT socket so one certificate must
// answer every tenant. caLocation is what the phone verifies the SERVER against.
// Put the tenant CA there and every handshake fails, at the client, where we
// never see it.
func TestTheTruststoreCarriesTheFrontsChainAndNotTheTenantCA(t *testing.T) {
	f := build(t, nil)
	_, serverCA, serverLeaf := frontTrust(t)

	for name, data := range map[string][]byte{
		"atak": unzip(t, unzip(t, f.atak.Data)["80b828699e074a239066d454a76284eb/phone01.zip"])["5c2bfcae3d98c9f4d262172df99ebac5/truststore-root.p12"],
		"itak": unzip(t, f.itak.Data)["truststore-root.p12"],
	} {
		trust, err := pkcs12.DecodeTrustStore(data, pkcs12.DefaultPassword)
		if err != nil {
			t.Fatalf("%s truststore does not decode: %v", name, err)
		}
		var hasServerCA, hasTenantCA, hasLeaf bool
		for _, c := range trust {
			switch {
			case c.Equal(serverCA):
				hasServerCA = true
			case c.Equal(f.caCert):
				hasTenantCA = true
			case c.Equal(serverLeaf):
				hasLeaf = true
			}
		}
		if !hasServerCA {
			t.Errorf("%s truststore does not carry the front's issuing CA; the phone cannot verify the server", name)
		}
		if hasTenantCA {
			t.Errorf("%s truststore carries the TENANT CA; that is the client side, not the server side", name)
		}
		// Pinning the leaf would work until the certificate is renewed, which for
		// Let's Encrypt is every 60 days, and then break every phone at once.
		if hasLeaf {
			t.Errorf("%s truststore pins the front's LEAF; it must anchor on the CA", name)
		}
	}
}

// The client p12 keeps the TENANT chain, which is the other half of the same
// distinction: that is the chain the phone's own certificate belongs to.
func TestTheClientKeystoreKeepsTheTenantChain(t *testing.T) {
	f := build(t, nil)
	inner := unzip(t, unzip(t, f.atak.Data)["80b828699e074a239066d454a76284eb/phone01.zip"])
	_, _, cas, err := pkcs12.DecodeChain(inner["5c2bfcae3d98c9f4d262172df99ebac5/phone01.p12"], pkcs12.DefaultPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(cas) != 1 || !cas[0].Equal(f.caCert) {
		t.Errorf("the client p12 carries %d CAs, want the tenant CA", len(cas))
	}
}

func TestAPackageWithNoServerTrustIsRefused(t *testing.T) {
	base := build(t, nil).in
	in := base
	in.ServerTrustPEM = nil
	_, _, err := Build(in)
	if err == nil {
		t.Fatal("built a package with nothing for the phone to trust the server with")
	}
	if !strings.Contains(err.Error(), "server trust") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// A private deployment may hand over a single self-signed server certificate. It
// is not marked as a CA, and trusting exactly it is a coherent choice, so that
// must still produce a package rather than an error.
func TestASingleSelfSignedServerCertificateIsUsableAsTheAnchor(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "tak.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}, &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "tak.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	selfSigned, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f := build(t, func(in *Input) {
		in.ServerTrustPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	})
	trust, err := pkcs12.DecodeTrustStore(
		unzip(t, f.itak.Data)["truststore-root.p12"], pkcs12.DefaultPassword)
	if err != nil {
		t.Fatalf("truststore does not decode: %v", err)
	}
	if len(trust) != 1 || !trust[0].Equal(selfSigned) {
		t.Errorf("a self-signed server certificate was not used as the anchor")
	}
}

// TestTheEncoderIsTheLegacyOne is the assertion a handset would otherwise make
// for us. pkcs12.Modern2023 needs Java 12+, ATAK is Android Java on a long tail
// of devices, and upstream OpenTAKServer uses `openssl pkcs12 -legacy`. Swapping
// the encoder changes nothing a normal test can see: the file still decodes in
// Go, and is refused on the phone. So check the algorithm in the DER.
func TestTheEncoderIsTheLegacyOne(t *testing.T) {
	f := build(t, nil)
	inner := unzip(t, unzip(t, f.atak.Data)["80b828699e074a239066d454a76284eb/phone01.zip"])

	der := func(oid asn1.ObjectIdentifier) []byte {
		b, err := asn1.Marshal(oid)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// pbeWithSHAAnd3-KeyTripleDES-CBC, what LegacyDES uses.
	legacy3DES := der(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3})
	// PBES2, which Modern2023/Modern2026 use and legacy never does.
	pbes2 := der(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13})

	for _, name := range []string{
		"5c2bfcae3d98c9f4d262172df99ebac5/phone01.p12",
		"5c2bfcae3d98c9f4d262172df99ebac5/truststore-root.p12",
	} {
		blob := inner[name]
		if !bytes.Contains(blob, legacy3DES) {
			t.Errorf("%s is not encrypted with the legacy 3DES PBE; a modern p12 is refused by older ATAK", name)
		}
		if bytes.Contains(blob, pbes2) {
			t.Errorf("%s uses PBES2, so it came from a Modern encoder", name)
		}
	}
}

// TestAHostileDescriptionCannotBreakTheFile is why the XML is marshalled rather
// than templated. Upstream uses Jinja templates with no escaping, so a server
// name containing & produces a file the client cannot parse.
func TestAHostileDescriptionCannotBreakTheFile(t *testing.T) {
	f := build(t, func(in *Input) {
		in.Description = `Tom & Jerry <"Ops">`
	})
	pref := innerPref(t, f)
	if strings.Contains(pref, "Tom & Jerry") {
		t.Error("the ampersand was written raw; this file will not parse on a client")
	}
	var doc prefDoc
	if err := xml.Unmarshal([]byte(pref), &doc); err != nil {
		t.Fatalf("pref with a hostile description does not parse: %v", err)
	}
	var found string
	for _, e := range doc.Groups[0].Entries {
		if e.Key == "description0" {
			found = e.Value
		}
	}
	if found != `Tom & Jerry <"Ops">` {
		t.Errorf("description round-tripped as %q", found)
	}
}

func TestEachPackageGetsItsOwnUID(t *testing.T) {
	a := build(t, nil)
	b := build(t, nil)
	get := func(f fixture) string {
		var man manifestDoc
		if err := xml.Unmarshal(unzip(t, f.atak.Data)["MANIFEST/manifest.xml"], &man); err != nil {
			t.Fatal(err)
		}
		for _, p := range man.Configuration {
			if p.Name == "uid" {
				return p.Value
			}
		}
		return ""
	}
	if x, y := get(a), get(b); x == "" || x == y {
		t.Errorf("two packages share uid %q; ATAK keys imports by it", x)
	}
}

// TestTheUsernameRuleIsEnforcedHere: the operator denies a username it cannot
// use, and a denial arrives after the customer has been promised a package.
func TestTheUsernameRuleIsEnforcedHere(t *testing.T) {
	for _, bad := range []string{"phone-01", "Phone01", "ph", "phone.01", "a@b", strings.Repeat("x", 33), ""} {
		_, _, err := Build(Input{
			Host: "h", Port: 8089, Username: bad,
			KeyPEM: []byte("x"), CertPEM: []byte("x"), CACertPEM: []byte("x"),
		})
		if err == nil {
			t.Errorf("username %q was accepted; OpenTAKServer would reject it", bad)
		} else if !strings.Contains(err.Error(), "username") {
			t.Errorf("username %q refused with an unhelpful error: %v", bad, err)
		}
	}
}

// TestTheCertificateMustBelongToTheUsername: the server looks the common name up
// as an OTS account, so a package pairing one user's name with another's
// certificate connects as nobody and is dropped.
func TestTheCertificateMustBelongToTheUsername(t *testing.T) {
	f := build(t, nil)
	in := f.in
	in.Username = "phone02"
	_, _, err := Build(in)
	if err == nil {
		t.Fatal("a certificate whose CN is not the username was accepted")
	}
	if !strings.Contains(err.Error(), "CN") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
}

func TestMissingMaterialIsRefused(t *testing.T) {
	base := build(t, nil).in
	for name, mutate := range map[string]func(*Input){
		"no key":  func(in *Input) { in.KeyPEM = nil },
		"no cert": func(in *Input) { in.CertPEM = nil },
		"no ca":   func(in *Input) { in.CACertPEM = nil },
		"no host": func(in *Input) { in.Host = "" },
		"no port": func(in *Input) { in.Port = 0 },
		"bad key": func(in *Input) { in.KeyPEM = []byte("-----BEGIN PRIVATE KEY-----\nZm9v\n-----END PRIVATE KEY-----\n") },
	} {
		in := base
		mutate(&in)
		if _, _, err := Build(in); err == nil {
			t.Errorf("%s: built a package anyway", name)
		}
	}
}

func TestSanitiseUsername(t *testing.T) {
	for in, want := range map[string]string{
		"Kyriakos.P@meshsat.net": "kyriakospmeshsatnet",
		"phone-01":               "phone01",
		"ATAK_User 2":            "atakuser2",
	} {
		got, ok := SanitiseUsername(in)
		if !ok || got != want {
			t.Errorf("SanitiseUsername(%q) = %q,%v want %q,true", in, got, ok, want)
		}
	}
	for _, in := range []string{"--", "@@", "ab", ""} {
		if got, ok := SanitiseUsername(in); ok {
			t.Errorf("SanitiseUsername(%q) = %q,true; nothing usable is left", in, got)
		}
	}
}

func TestBundleDescribeNeverLeaksBytes(t *testing.T) {
	f := build(t, nil)
	d := f.atak.Describe()
	if !strings.Contains(d, "phone01_CONFIG.zip") {
		t.Errorf("Describe() = %q, want the filename", d)
	}
	if strings.Contains(d, "PRIVATE") || len(d) > 80 {
		t.Errorf("Describe() looks like it carries content: %q", d)
	}
}
