// Command certgen issues MCP test certificates signed by the MCP test sub-CA.
//
// It exists because the original issue-cert.cmd workflow depends on openssl,
// which does not run in this environment. The sub-CA key is read from a PKCS#12
// bundle that has to be exported from the Identity Registry JKS keystore first:
//
//	keytool -importkeystore \
//	  -srckeystore <identity-registry>/<ca-keystore>.jks -srcstoretype JKS \
//	  -srcstorepass <pass> -srcalias urn:mrn:mcp:ca:idp1:mcp-idreg -srckeypass <pass> \
//	  -destkeystore subca-legacy.p12 -deststoretype PKCS12 -deststorepass <pass> \
//	  -J-Dkeystore.pkcs12.keyProtectionAlgorithm=PBEWithSHA1AndDESede \
//	  -J-Dkeystore.pkcs12.certProtectionAlgorithm=PBEWithSHA1AndRC2_40 \
//	  -J-Dkeystore.pkcs12.macAlgorithm=HmacPBESHA1
//
// The legacy PBE algorithms keep the export readable by both the Go and the Java
// side. A default keytool export uses AES/PBES2, which older PKCS#12 parsers
// cannot decrypt.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// uidOID is the UID attribute from RFC 4519, which the MMS components use to
// carry the MRN of a certificate holder.
var uidOID = []int{0, 9, 2342, 19200300, 100, 1, 1}

func main() {
	subCA := flag.String("subca", "", "Path to the PKCS#12 bundle holding the sub-CA key and certificate.")
	subCAPass := flag.String("subca-pass", "", "Password of the sub-CA PKCS#12 bundle.")
	name := flag.String("name", "", "Short name of the certificate, e.g. vde-ter-er-01. Used as the CN and the file name.")
	mrn := flag.String("mrn", "", "MRN to put in the UID attribute. Defaults to urn:mrn:mcp:mms:idp1:bootstrap:<name>.")
	kind := flag.String("kind", "er", "Certificate kind: er (server and client auth) or agent (client auth only).")
	out := flag.String("out", "", "Directory to write <name>.crt and <name>.key into.")
	days := flag.Int("days", 180, "Validity period in days.")
	flag.Parse()

	if *subCA == "" || *name == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "certgen: -subca, -name and -out are required")
		os.Exit(2)
	}
	if *mrn == "" {
		prefix := "urn:mrn:mcp:mms:idp1:bootstrap:"
		if *kind == "agent" {
			prefix = "urn:mrn:mcp:entity:idp1:bootstrap:"
		}
		*mrn = prefix + *name
	}

	if err := run(*subCA, *subCAPass, *name, *mrn, *kind, *out, *days); err != nil {
		fmt.Fprintf(os.Stderr, "certgen: %v\n", err)
		os.Exit(1)
	}
}

func run(subCAPath, subCAPass, name, mrn, kind, outDir string, days int) error {
	caKey, caCert, err := loadSubCA(subCAPath, subCAPass)
	if err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("could not generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("could not generate serial number: %w", err)
	}

	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	if kind == "agent" {
		usage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Country:            []string{"DK"},
			Organization:       []string{"urn:mrn:mcp:org:idp1:bootstrap"},
			OrganizationalUnit: []string{"service"},
			CommonName:         name,
			ExtraNames: []pkix.AttributeTypeAndValue{
				{Type: uidOID, Value: mrn},
			},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           usage,
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("could not sign certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("could not parse the freshly signed certificate: %w", err)
	}
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("signed certificate does not verify against the sub-CA: %w", err)
	}
	if got := mrnFromCert(cert); got != mrn {
		return fmt.Errorf("issued certificate carries MRN %q, expected %q", got, mrn)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", outDir, err)
	}

	certPath := filepath.Join(outDir, name+".crt")
	keyPath := filepath.Join(outDir, name+".key")

	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("could not marshal private key: %w", err)
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyDER); err != nil {
		return err
	}

	fmt.Printf("issued %s\n  MRN:    %s\n  kind:   %s\n  valid:  %s\n  cert:   %s\n  key:    %s\n",
		name, mrn, kind, cert.NotAfter.Format(time.RFC3339), certPath, keyPath)
	return nil
}

func loadSubCA(path, password string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("could not read %s: %w", path, err)
	}

	key, cert, _, err := pkcs12.DecodeChain(data, password)
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode %s: %w", path, err)
	}

	caKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("sub-CA key in %s is a %T, only ECDSA keys are supported", path, key)
	}
	if !cert.IsCA {
		return nil, nil, errors.New("the certificate in " + path + " is not a CA certificate")
	}
	return caKey, cert, nil
}

func mrnFromCert(cert *x509.Certificate) string {
	for _, name := range cert.Subject.Names {
		if name.Type.Equal(uidOID) {
			if value, ok := name.Value.(string); ok {
				return value
			}
		}
	}
	return ""
}

func writePEM(path, blockType string, der []byte) error {
	var b strings.Builder
	if err := pem.Encode(&b, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return fmt.Errorf("could not encode %s: %w", blockType, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("could not write %s: %w", path, err)
	}
	return nil
}
