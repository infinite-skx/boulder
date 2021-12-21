package notmain

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"hash"
	"os"
	"os/user"
	"sort"
	"strconv"
	"sync"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/db"
	berrors "github.com/letsencrypt/boulder/errors"
	"github.com/letsencrypt/boulder/features"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	rapb "github.com/letsencrypt/boulder/ra/proto"
	"github.com/letsencrypt/boulder/revocation"
	"github.com/letsencrypt/boulder/sa"
	sapb "github.com/letsencrypt/boulder/sa/proto"
)

const usageString = `
usage:
admin-revoker serial-revoke --config <path> <serial> <reason-code>
admin-revoker batched-serial-revoke --config <path> <serial-file-path> <reason-code> <parallelism>
admin-revoker reg-revoke --config <path> <registration-id> <reason-code>
admin-revoker list-reasons --config <path>
admin-revoker list-reasons --config <path> <private-key-path> <reason-code>

command descriptions:
  serial-revoke         Revoke a single certificate by the hex serial number
  batched-serial-revoke Revokes all certificates contained in a file of hex serial numbers
  reg-revoke            Revoke all certificates associated with a registration ID
  list-reasons          List all revocation reason codes
  block-key             Adds the public key derived from the provided private key to the
                        blocked keys table. <private-key-path> is expected to be the path
						to a PEM formatted file containing an RSA or ECDSA private key in
						a PKCS #1, PKCS# 8, or SEC 1 container.

args:
  config    File path to the configuration file for this service
`

type config struct {
	Revoker struct {
		DB cmd.DBConfig
		// Similarly, the Revoker needs a TLSConfig to set up its GRPC client certs,
		// but doesn't get the TLS field from ServiceConfig, so declares its own.
		TLS cmd.TLSConfig

		RAService *cmd.GRPCClientConfig
		SAService *cmd.GRPCClientConfig

		Features map[string]bool
	}

	Syslog cmd.SyslogConfig
}

type revoker struct {
	rac   rapb.RegistrationAuthorityClient
	sac   sapb.StorageAuthorityClient
	dbMap *db.WrappedMap
	log   blog.Logger
}

func newRevoker(c config) *revoker {
	logger := cmd.NewLogger(c.Syslog)

	tlsConfig, err := c.Revoker.TLS.Load()
	cmd.FailOnError(err, "TLS config")

	clk := cmd.Clock()

	clientMetrics := bgrpc.NewClientMetrics(metrics.NoopRegisterer)
	raConn, err := bgrpc.ClientSetup(c.Revoker.RAService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Failed to load credentials and create gRPC connection to RA")
	rac := rapb.NewRegistrationAuthorityClient(raConn)

	dbURL, err := c.Revoker.DB.URL()
	cmd.FailOnError(err, "Couldn't load DB URL")
	dbSettings := sa.DbSettings{
		MaxOpenConns:    c.Revoker.DB.MaxOpenConns,
		MaxIdleConns:    c.Revoker.DB.MaxIdleConns,
		ConnMaxLifetime: c.Revoker.DB.ConnMaxLifetime.Duration,
		ConnMaxIdleTime: c.Revoker.DB.ConnMaxIdleTime.Duration,
	}
	dbMap, err := sa.NewDbMap(dbURL, dbSettings)
	cmd.FailOnError(err, "Couldn't setup database connection")

	saConn, err := bgrpc.ClientSetup(c.Revoker.SAService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Failed to load credentials and create gRPC connection to SA")
	sac := sapb.NewStorageAuthorityClient(saConn)

	return &revoker{
		rac:   rac,
		sac:   sac,
		dbMap: dbMap,
		log:   logger,
	}
}

func (r *revoker) revokeCertificate(ctx context.Context, certObj core.Certificate, reasonCode revocation.Reason) error {
	if reasonCode < 0 || reasonCode == 7 || reasonCode > 10 {
		panic(fmt.Sprintf("Invalid reason code: %d", reasonCode))
	}
	u, err := user.Current()
	if err != nil {
		return err
	}

	var req *rapb.AdministrativelyRevokeCertificateRequest
	if certObj.DER != nil {
		cert, err := x509.ParseCertificate(certObj.DER)
		if err != nil {
			return err
		}
		req = &rapb.AdministrativelyRevokeCertificateRequest{
			Cert:      cert.Raw,
			Code:      int64(reasonCode),
			AdminName: u.Username,
		}
	} else {
		req = &rapb.AdministrativelyRevokeCertificateRequest{
			Serial:    certObj.Serial,
			Code:      int64(reasonCode),
			AdminName: u.Username,
		}
	}
	_, err = r.rac.AdministrativelyRevokeCertificate(ctx, req)
	if err != nil {
		return err
	}
	r.log.Infof("Revoked certificate %s with reason '%s'", certObj.Serial, revocation.ReasonToString[reasonCode])
	return nil
}

func (r *revoker) revokeBySerial(ctx context.Context, serial string, reasonCode revocation.Reason) error {
	certObj, err := sa.SelectPrecertificate(r.dbMap, serial)
	if err != nil {
		if db.IsNoRows(err) {
			return berrors.NotFoundError("precertificate with serial %q not found", serial)
		}
		return err
	}
	return r.revokeCertificate(ctx, certObj, reasonCode)
}

func (r *revoker) revokeBySerialBatch(ctx context.Context, serialPath string, reasonCode revocation.Reason, parallelism int) error {
	file, err := os.Open(serialPath)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(file)
	if err != nil {
		return err
	}

	wg := new(sync.WaitGroup)
	work := make(chan string, parallelism)
	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for serial := range work {
				// handle newlines gracefully
				if serial == "" {
					continue
				}
				err := r.revokeBySerial(ctx, serial, reasonCode)
				if err != nil {
					r.log.Errf("failed to revoke %q: %s", serial, err)
				}
			}
		}()
	}

	for scanner.Scan() {
		serial := scanner.Text()
		if serial == "" {
			continue
		}
		work <- serial
	}
	close(work)
	wg.Wait()

	return nil
}

func (r *revoker) revokeByReg(ctx context.Context, regID int64, reasonCode revocation.Reason) error {
	_, err := r.sac.GetRegistration(ctx, &sapb.RegistrationID{Id: regID})
	if err != nil {
		return fmt.Errorf("couldn't fetch registration: %w", err)
	}

	certObjs, err := sa.SelectPrecertificates(r.dbMap, "WHERE registrationID = :regID", map[string]interface{}{"regID": regID})
	if err != nil {
		return err
	}
	for _, certObj := range certObjs {
		err = r.revokeCertificate(ctx, certObj.Certificate, reasonCode)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *revoker) revokeMalformedBySerial(ctx context.Context, serial string, reasonCode revocation.Reason) error {
	return r.revokeCertificate(ctx, core.Certificate{Serial: serial}, reasonCode)
}

// This abstraction is needed so that we can use sort.Sort below
type revocationCodes []revocation.Reason

func (rc revocationCodes) Len() int           { return len(rc) }
func (rc revocationCodes) Less(i, j int) bool { return rc[i] < rc[j] }
func (rc revocationCodes) Swap(i, j int)      { rc[i], rc[j] = rc[j], rc[i] }

// loadPrivateKey decodes and parses a private key from the provided
// `keyContents` and returns the private key as `crypto.Signer`. `keyContents`
// is expected to be a PEM formatted RSA or ECDSA private key in a PKCS #1,
// PKCS# 8, or SEC 1 container.
func loadPrivateKey(keyContents []byte) (crypto.Signer, error) {
	// Attempt to find and decode the first PEM block.
	block, _ := pem.Decode(keyContents)
	if block == nil {
		return nil, errors.New("does not contain a PEM formatted block")
	}

	// Attempt to parse the PEM block as a private key in a PKCS #1 container.
	rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err == nil {
		return rsaKey, nil
	}

	// Attempt to parse the PEM block as a private key in a PKCS #8 container.
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		switch k := key.(type) {
		case *rsa.PrivateKey:
			return k, nil

		case *ecdsa.PrivateKey:
			return k, nil
		}
	}

	// Attempt to parse the PEM block as a private key in a SEC 1 container.
	ecdsaKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err == nil {
		return ecdsaKey, nil
	}

	// No private key could be parsed from the PEM block.
	return nil, errors.New("cannot parse a private key from the provided PEM file")
}

func verifyRSAKeyPair(privKey *rsa.PrivateKey, pubKey *rsa.PublicKey, msgHash hash.Hash) error {
	// privateKeyRSA2, err := rsa.GenerateKey(rand.Reader, 2048)
	// cmd.FailOnError(err, "Failed to generate random RSA Key")

	signatureRSA, err := rsa.SignPSS(rand.Reader, privKey, crypto.SHA256, msgHash.Sum(nil), nil)
	if err != nil {
		return fmt.Errorf("failed to sign using the provided RSA private key: %s", err)
	}

	err = rsa.VerifyPSS(pubKey, crypto.SHA256, msgHash.Sum(nil), signatureRSA, nil)
	if err != nil {
		return fmt.Errorf("the provided ECDSA private key failed signature verification: %s", err)
	}
	return err
}

func verifyECDSAKeyPair(privKey *ecdsa.PrivateKey, pubKey *ecdsa.PublicKey, msgHash hash.Hash) error {
	// privateKeyECDSA2, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// cmd.FailOnError(err, "Failed to generate random ECDSA Key")

	r, s, err := ecdsa.Sign(rand.Reader, privKey, msgHash.Sum(nil))
	if err != nil {
		return fmt.Errorf("failed to sign using the provided ECDSA private key: %s", err)
	}

	verify := ecdsa.Verify(pubKey, msgHash.Sum(nil), r, s)
	if !verify {
		return errors.New("the provided ECDSA private key failed signature verification")
	}
	return err
}

// verifyPrivateKey accepts a private key in the form of a `crypto.Signer` and
// performs signing and verification for both RSA and ECDSA private keys.
// Returns a nil error if signing and verification has been completed
// successfully.
func verifyPrivateKey(privateKey crypto.Signer) error {
	msgHash := sha256.New()
	_, err := msgHash.Write([]byte("verifiable"))
	if err != nil {
		return fmt.Errorf("failed to hash 'verifiable' message: %s", err)
	}

	privateKeyRSA, ok := privateKey.(*rsa.PrivateKey)
	if ok {
		return verifyRSAKeyPair(privateKeyRSA, &privateKeyRSA.PublicKey, msgHash)
	} else {
		privateKeyECDSA, ok := privateKey.(*ecdsa.PrivateKey)
		if ok {
			return verifyECDSAKeyPair(privateKeyECDSA, &privateKeyECDSA.PublicKey, msgHash)
		}
	}
	// This should not happen.
	return errors.New("the provided private key could not be asserted to ECDSA or RSA")
}

func main() {
	usage := func() {
		fmt.Fprint(os.Stderr, usageString)
		os.Exit(1)
	}
	if len(os.Args) <= 2 {
		usage()
	}

	command := os.Args[1]
	flagSet := flag.NewFlagSet(command, flag.ContinueOnError)
	configFile := flagSet.String("config", "", "File path to the configuration file for this service")
	err := flagSet.Parse(os.Args[2:])
	cmd.FailOnError(err, "Error parsing flagset")

	if *configFile == "" {
		usage()
	}

	var c config
	err = cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")
	err = features.Set(c.Revoker.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	ctx := context.Background()
	r := newRevoker(c)
	defer r.log.AuditPanic()

	args := flagSet.Args()
	switch {
	case command == "serial-revoke" && len(args) == 2:
		// 1: serial,  2: reasonCode
		serial := args[0]
		reasonCode, err := strconv.Atoi(args[1])
		cmd.FailOnError(err, "Reason code argument must be an integer")

		err = r.revokeBySerial(ctx, serial, revocation.Reason(reasonCode))
		cmd.FailOnError(err, "Couldn't revoke certificate by serial")

	case command == "batched-serial-revoke" && len(args) == 3:
		// 1: serial file path,  2: reasonCode, 3: parallelism
		serialPath := args[0]
		reasonCode, err := strconv.Atoi(args[1])
		cmd.FailOnError(err, "Reason code argument must be an integer")
		parallelism, err := strconv.Atoi(args[2])
		cmd.FailOnError(err, "parallelism argument must be an integer")
		if parallelism < 1 {
			cmd.Fail("parallelism argument must be >= 1")
		}

		err = r.revokeBySerialBatch(ctx, serialPath, revocation.Reason(reasonCode), parallelism)
		cmd.FailOnError(err, "Batch revocation failed")

	case command == "reg-revoke" && len(args) == 2:
		// 1: registration ID,  2: reasonCode
		regID, err := strconv.ParseInt(args[0], 10, 64)
		cmd.FailOnError(err, "Registration ID argument must be an integer")
		reasonCode, err := strconv.Atoi(args[1])
		cmd.FailOnError(err, "Reason code argument must be an integer")

		err = r.revokeByReg(ctx, regID, revocation.Reason(reasonCode))
		cmd.FailOnError(err, "Couldn't revoke certificate by registration")

	case command == "malformed-revoke" && len(args) == 3:
		// 1: serial, 2: reasonCode
		serial := args[0]
		reasonCode, err := strconv.Atoi(args[1])
		cmd.FailOnError(err, "Reason code argument must be an integer")

		err = r.revokeMalformedBySerial(ctx, serial, revocation.Reason(reasonCode))
		cmd.FailOnError(err, "Couldn't revoke certificate by serial")

	case command == "list-reasons":
		var codes revocationCodes
		for k := range revocation.ReasonToString {
			codes = append(codes, k)
		}
		sort.Sort(codes)
		fmt.Printf("Revocation reason codes\n-----------------------\n\n")
		for _, k := range codes {
			fmt.Printf("%d: %s\n", k, revocation.ReasonToString[k])
		}

	case command == "block-key" && len(args) == 2:
		// 1: keyPath, 2: reasonCode
		keyPath := args[0]

		reasonCode, err := strconv.Atoi(args[1])
		cmd.FailOnError(err, "Reason code argument must be an integer")
		fmt.Println(reasonCode)

		keyContents, err := os.ReadFile(keyPath)
		cmd.FailOnError(err, fmt.Sprintf("Cannot load file %q", keyPath))

		privateKey, err := loadPrivateKey(keyContents)
		cmd.FailOnError(err, fmt.Sprintf("Cannot parse private key from file %q", keyPath))

		err = verifyPrivateKey(privateKey)
		cmd.FailOnError(err, "While attempting to verify")
		r.log.AuditInfo("the provided private key has been successfully verified")

	default:
		usage()
	}
}

func init() {
	cmd.RegisterCommand("admin-revoker", main)
}
