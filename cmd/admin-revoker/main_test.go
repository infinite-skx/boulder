package notmain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/jmhodges/clock"
	akamaipb "github.com/letsencrypt/boulder/akamai/proto"
	capb "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/goodkey"
	"github.com/letsencrypt/boulder/issuance"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/letsencrypt/boulder/mocks"
	"github.com/letsencrypt/boulder/ra"
	"github.com/letsencrypt/boulder/sa"
	sapb "github.com/letsencrypt/boulder/sa/proto"
	"github.com/letsencrypt/boulder/sa/satest"
	"github.com/letsencrypt/boulder/test"
	ira "github.com/letsencrypt/boulder/test/inmem/ra"
	isa "github.com/letsencrypt/boulder/test/inmem/sa"
	"github.com/letsencrypt/boulder/test/vars"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type mockCA struct {
	mocks.MockCA
}

func (ca *mockCA) GenerateOCSP(context.Context, *capb.GenerateOCSPRequest, ...grpc.CallOption) (*capb.OCSPResponse, error) {
	return &capb.OCSPResponse{Response: []byte("fakeocspbytes")}, nil
}

type mockPurger struct{}

func (mp *mockPurger) Purge(context.Context, *akamaipb.PurgeRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func TestRevokeBatch(t *testing.T) {
	log := blog.UseMock()
	fc := clock.NewFake()
	// Set to some non-zero time.
	fc.Set(time.Date(2015, 3, 4, 5, 0, 0, 0, time.UTC))
	dbMap, err := sa.NewDbMap(vars.DBConnSA, sa.DbSettings{})
	if err != nil {
		t.Fatalf("Failed to create dbMap: %s", err)
	}
	ssa, err := sa.NewSQLStorageAuthority(dbMap, dbMap, fc, log, metrics.NoopRegisterer, 1)
	if err != nil {
		t.Fatalf("Failed to create SA: %s", err)
	}
	defer test.ResetSATestDatabase(t)
	reg := satest.CreateWorkingRegistration(t, isa.SA{Impl: ssa})

	issuer, err := issuance.LoadCertificate("../../test/hierarchy/int-r3.cert.pem")
	test.AssertNotError(t, err, "Failed to load test issuer")
	signer, err := test.LoadSigner("../../test/hierarchy/int-r3.key.pem")
	test.AssertNotError(t, err, "failed to load test signer")

	ra := ra.NewRegistrationAuthorityImpl(fc,
		log,
		metrics.NoopRegisterer,
		1,
		goodkey.KeyPolicy{},
		100,
		true,
		300*24*time.Hour,
		7*24*time.Hour,
		nil,
		nil,
		0,
		nil,
		&mockPurger{},
		[]*issuance.Certificate{issuer},
	)
	ra.SA = isa.SA{Impl: ssa}
	ra.CA = &mockCA{}
	rac := ira.RA{Impl: ra}

	r := revoker{
		rac:   rac,
		sac:   isa.SA{Impl: ssa},
		dbMap: dbMap,
		clk:   fc,
		log:   log,
	}

	serialFile, err := ioutil.TempFile("", "serials")
	test.AssertNotError(t, err, "failed to open temp file")
	defer os.Remove(serialFile.Name())

	serials := []*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3)}
	for _, serial := range serials {
		template := &x509.Certificate{
			SerialNumber: serial,
			DNSNames:     []string{"asd"},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, issuer.Certificate, signer.Public(), signer)
		test.AssertNotError(t, err, "failed to generate test cert")
		_, err = ssa.AddPrecertificate(context.Background(), &sapb.AddCertificateRequest{
			Der:      der,
			RegID:    reg.Id,
			Issued:   time.Now().UnixNano(),
			IssuerID: 1,
		})
		test.AssertNotError(t, err, "failed to add test cert")
		_, err = ssa.AddCertificate(context.Background(), &sapb.AddCertificateRequest{
			Der:    der,
			RegID:  reg.Id,
			Issued: time.Now().UnixNano(),
		})
		test.AssertNotError(t, err, "failed to add test cert")
		_, err = serialFile.WriteString(fmt.Sprintf("%s\n", core.SerialToString(serial)))
		test.AssertNotError(t, err, "failed to write serial to temp file")
	}

	err = r.revokeBySerialBatch(context.Background(), serialFile.Name(), 0, 2)
	test.AssertNotError(t, err, "revokeBatch failed")

	for _, serial := range serials {
		status, err := ssa.GetCertificateStatus(context.Background(), &sapb.Serial{Serial: core.SerialToString(serial)})
		test.AssertNotError(t, err, "failed to retrieve certificate status")
		test.AssertEquals(t, core.OCSPStatus(status.Status), core.OCSPStatusRevoked)
	}
}

func TestVerifyRSAKeyPair(t *testing.T) {
	msgHash := sha256.New()
	_, err := msgHash.Write([]byte("verifiable"))
	test.AssertNotError(t, err, "Failed to hash 'verifiable' message: %s")

	privKey1, err := rsa.GenerateKey(rand.Reader, 2048)
	test.AssertNotError(t, err, "Failed while generating test key 1.")

	err = verifyRSAKeyPair(privKey1, &privKey1.PublicKey, msgHash)
	test.AssertNotError(t, err, "Failed to verify valid key pair.")

	privKey2, err := rsa.GenerateKey(rand.Reader, 2048)
	test.AssertNotError(t, err, "Failed while generating test key 2.")

	err = verifyRSAKeyPair(privKey1, &privKey2.PublicKey, msgHash)
	test.AssertError(t, err, "Failed to detect invalid key pair.")
}

func TestVerifyECDSAKeyPair(t *testing.T) {
	msgHash := sha256.New()
	_, err := msgHash.Write([]byte("verifiable"))
	test.AssertNotError(t, err, "Failed to hash 'verifiable' message: %s")

	privKey1, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.AssertNotError(t, err, "Failed while generating test key 1.")

	err = verifyECDSAKeyPair(privKey1, &privKey1.PublicKey, msgHash)
	test.AssertNotError(t, err, "Failed to verify valid key pair")

	privKey2, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	test.AssertNotError(t, err, "Failed while generating test key 2.")

	err = verifyECDSAKeyPair(privKey1, &privKey2.PublicKey, msgHash)
	test.AssertError(t, err, "Failed to detect invalid key pair.")
}

func TestCountCertsMatchingSPKIHash(t *testing.T) {
	log := blog.UseMock()
	fc := clock.NewFake()

	// Set to some non-zero time.
	fc.Set(time.Date(2015, 3, 4, 5, 0, 0, 0, time.UTC))
	fc.Set(time.Now())
	dbMap, err := sa.NewDbMap(vars.DBConnSA, sa.DbSettings{})
	if err != nil {
		t.Fatalf("Failed to create dbMap: %s", err)
	}
	ssa, err := sa.NewSQLStorageAuthority(dbMap, dbMap, fc, log, metrics.NoopRegisterer, 1)
	if err != nil {
		t.Fatalf("Failed to create SA: %s", err)
	}
	//defer test.ResetSATestDatabase(t)
	reg := satest.CreateWorkingRegistration(t, isa.SA{Impl: ssa})

	issuer, err := issuance.LoadCertificate("../../test/hierarchy/int-r3.cert.pem")
	test.AssertNotError(t, err, "Failed to load test issuer")
	signer, err := test.LoadSigner("../../test/hierarchy/int-r3.key.pem")
	test.AssertNotError(t, err, "failed to load test signer")

	ra := ra.NewRegistrationAuthorityImpl(fc,
		log,
		metrics.NoopRegisterer,
		1,
		goodkey.KeyPolicy{},
		100,
		true,
		300*24*time.Hour,
		7*24*time.Hour,
		nil,
		nil,
		0,
		nil,
		&mockPurger{},
		[]*issuance.Certificate{issuer},
	)
	ra.SA = isa.SA{Impl: ssa}
	ra.CA = &mockCA{}
	rac := ira.RA{Impl: ra}

	r := revoker{
		rac:   rac,
		sac:   isa.SA{Impl: ssa},
		dbMap: dbMap,
		clk:   fc,
		log:   log,
	}

	serial := big.NewInt(1)
	template := &x509.Certificate{
		SerialNumber: serial,
		DNSNames:     []string{"asd"},
	}
	rawCert, err := x509.CreateCertificate(rand.Reader, template, issuer.Certificate, signer.Public(), signer)
	test.AssertNotError(t, err, "failed to generate test cert")

	_, err = ssa.AddPrecertificate(context.Background(), &sapb.AddCertificateRequest{
		Der:      rawCert,
		RegID:    reg.Id,
		Issued:   time.Now().UnixNano(),
		IssuerID: 1,
	})

	test.AssertNotError(t, err, "failed to add test cert")
	_, err = ssa.AddCertificate(context.Background(), &sapb.AddCertificateRequest{
		Der:    rawCert,
		RegID:  reg.Id,
		Issued: time.Now().UnixNano(),
	})
	test.AssertNotError(t, err, "failed to add test cert")
	cert, err := x509.ParseCertificate(rawCert)
	test.AssertNotError(t, err, "failed to parse test cert")

	spkiHash, err := getPublicKeySPKIHash(cert.PublicKey)
	test.AssertNotError(t, err, "failed to get SPKI hash for test cert")
	fmt.Println(spkiHash)

	serials := []*big.Int{big.NewInt(2), big.NewInt(3), big.NewInt(4)}
	for _, serial := range serials {
		template := &x509.Certificate{
			SerialNumber: serial,
			DNSNames:     []string{"asd"},
		}

		der, err := x509.CreateCertificate(rand.Reader, template, issuer.Certificate, signer.Public(), signer)
		test.AssertNotError(t, err, "failed to generate test cert")
		_, err = ssa.AddPrecertificate(context.Background(), &sapb.AddCertificateRequest{
			Der:      der,
			RegID:    reg.Id,
			Issued:   time.Now().UnixNano(),
			IssuerID: 1,
		})
		test.AssertNotError(t, err, "failed to add test cert")
		_, err = ssa.AddCertificate(context.Background(), &sapb.AddCertificateRequest{
			Der:    der,
			RegID:  reg.Id,
			Issued: time.Now().UnixNano(),
		})
		test.AssertNotError(t, err, "failed to add test cert")

		cert, err := x509.ParseCertificate(der)
		test.AssertNotError(t, err, "failed to parse test cert")

		spkiHash, err := getPublicKeySPKIHash(cert.PublicKey)
		test.AssertNotError(t, err, "failed to get SPKI hash for test cert")
		fmt.Println(spkiHash)
	}

	count, err := r.countCertsMatchingSPKIHash(spkiHash)
	fmt.Println(count)
	test.AssertNotError(t, err, "countCertsMatchingSPKIHash failed")
	test.AssertEquals(t, count, 1)
}
