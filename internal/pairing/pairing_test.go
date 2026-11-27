package pairing

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestPairTokenFromEnv(t *testing.T) {
	dir := t.TempDir()
	p, err := Load(dir, " given \n")
	if err != nil {
		t.Fatal(err)
	}
	if p.token != "given" {
		t.Fatalf("token %q", p.token)
	}
	if _, err := os.Stat(filepath.Join(dir, "pair-token")); !os.IsNotExist(err) {
		t.Fatal("pair-token must not be written when AGENT_PAIR_TOKEN is set")
	}
}

// denpa (Bun) が信頼の起点にできる形か: basicConstraints と keyUsage を付けない (AgentTls.cs の CreatePem)
func TestCertificateShape(t *testing.T) {
	p, err := Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(p.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if cert.BasicConstraintsValid || cert.KeyUsage != 0 || len(cert.SubjectKeyId) == 0 || len(cert.AuthorityKeyId) != 0 ||
		cert.Subject.CommonName != "denpa-agent" || cert.NotAfter.Year()-cert.NotBefore.Year() < 99 {
		t.Fatalf("unexpected certificate: %+v", cert)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Fatal(err)
	}
}
