package notmain

import (
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/honeycombio/beeline-go"
	akamaipb "github.com/letsencrypt/boulder/akamai/proto"
	capb "github.com/letsencrypt/boulder/ca/proto"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/ctpolicy"
	"github.com/letsencrypt/boulder/ctpolicy/ctconfig"
	"github.com/letsencrypt/boulder/features"
	"github.com/letsencrypt/boulder/goodkey"
	bgrpc "github.com/letsencrypt/boulder/grpc"
	"github.com/letsencrypt/boulder/issuance"
	"github.com/letsencrypt/boulder/policy"
	pubpb "github.com/letsencrypt/boulder/publisher/proto"
	"github.com/letsencrypt/boulder/ra"
	rapb "github.com/letsencrypt/boulder/ra/proto"
	sapb "github.com/letsencrypt/boulder/sa/proto"
	vapb "github.com/letsencrypt/boulder/va/proto"
)

type Config struct {
	RA struct {
		cmd.ServiceConfig
		cmd.HostnamePolicyConfig

		RateLimitPoliciesFilename string

		MaxContactsPerRegistration int

		SAService           *cmd.GRPCClientConfig
		VAService           *cmd.GRPCClientConfig
		CAService           *cmd.GRPCClientConfig
		PublisherService    *cmd.GRPCClientConfig
		AkamaiPurgerService *cmd.GRPCClientConfig

		MaxNames int

		// Controls behaviour of the RA when asked to create a new authz for
		// a name/regID that already has a valid authz. False preserves historic
		// behaviour and ignores the existing authz and creates a new one. True
		// instructs the RA to reuse the previously created authz in lieu of
		// creating another.
		ReuseValidAuthz bool

		// AuthorizationLifetimeDays defines how long authorizations will be
		// considered valid for. Given a value of 300 days when used with a 90-day
		// cert lifetime, this allows creation of certs that will cover a whole
		// year, plus a grace period of a month.
		AuthorizationLifetimeDays int

		// PendingAuthorizationLifetimeDays defines how long authorizations may be in
		// the pending state. If you can't respond to a challenge this quickly, then
		// you need to request a new challenge.
		PendingAuthorizationLifetimeDays int

		// GoodKey is an embedded config stanza for the goodkey library.
		GoodKey goodkey.Config

		// WeakKeyFile is DEPRECATED. Populate GoodKey.WeakKeyFile instead.
		// TODO(#5851): Remove this.
		WeakKeyFile string

		// WeakKeyFile is DEPRECATED. Populate GoodKey.BlockedKeyFile instead.
		// TODO(#5851): Remove this.
		BlockedKeyFile string

		OrderLifetime cmd.ConfigDuration

		// CTLogGroups contains groupings of CT logs which we want SCTs from.
		// When we retrieve SCTs we will submit the certificate to each log
		// in a group and the first SCT returned will be used. This allows
		// us to comply with Chrome CT policy which requires one SCT from a
		// Google log and one SCT from any other log included in their policy.
		CTLogGroups2 []ctconfig.CTGroup
		// InformationalCTLogs are a set of CT logs we will always submit to
		// but won't ever use the SCTs from. This may be because we want to
		// test them or because they are not yet approved by a browser/root
		// program but we still want our certs to end up there.
		InformationalCTLogs []ctconfig.LogDescription

		// IssuerCertPath is the path to the intermediate used to issue certificates.
		// It is used to generate OCSP URLs to purge at revocation time.
		// TODO(#5162): DEPRECATED. Remove this field entirely.
		IssuerCertPath string
		// IssuerCerts are paths to all intermediate certificates which may have
		// been used to issue certificates in the last 90 days. These are used to
		// generate OCSP URLs to purge during revocation.
		IssuerCerts []string

		Features map[string]bool
	}

	PA cmd.PAConfig

	Syslog  cmd.SyslogConfig
	Beeline cmd.BeelineConfig
}

func main() {
	grpcAddr := flag.String("addr", "", "gRPC listen address override")
	debugAddr := flag.String("debug-addr", "", "Debug server address override")
	configFile := flag.String("config", "", "File path to the configuration file for this service")
	flag.Parse()
	if *configFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	var c Config
	err := cmd.ReadConfigFile(*configFile, &c)
	cmd.FailOnError(err, "Reading JSON config file into config structure")

	err = features.Set(c.RA.Features)
	cmd.FailOnError(err, "Failed to set feature flags")

	if *grpcAddr != "" {
		c.RA.GRPC.Address = *grpcAddr
	}
	if *debugAddr != "" {
		c.RA.DebugAddr = *debugAddr
	}

	bc, err := c.Beeline.Load()
	cmd.FailOnError(err, "Failed to load Beeline config")
	beeline.Init(bc)
	defer beeline.Close()

	scope, logger := cmd.StatsAndLogging(c.Syslog, c.RA.DebugAddr)
	defer logger.AuditPanic()
	logger.Info(cmd.VersionString())

	// Validate PA config and set defaults if needed
	cmd.FailOnError(c.PA.CheckChallenges(), "Invalid PA configuration")

	pa, err := policy.New(c.PA.Challenges)
	cmd.FailOnError(err, "Couldn't create PA")

	if c.RA.HostnamePolicyFile == "" {
		cmd.Fail("HostnamePolicyFile must be provided.")
	}
	err = pa.SetHostnamePolicyFile(c.RA.HostnamePolicyFile)
	cmd.FailOnError(err, "Couldn't load hostname policy file")

	tlsConfig, err := c.RA.TLS.Load()
	cmd.FailOnError(err, "TLS config")

	clk := cmd.Clock()
	clientMetrics := bgrpc.NewClientMetrics(scope)

	vaConn, err := bgrpc.ClientSetup(c.RA.VAService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Unable to create VA client")
	vac := vapb.NewVAClient(vaConn)
	caaClient := vapb.NewCAAClient(vaConn)

	caConn, err := bgrpc.ClientSetup(c.RA.CAService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Unable to create CA client")
	cac := capb.NewCertificateAuthorityClient(caConn)

	saConn, err := bgrpc.ClientSetup(c.RA.SAService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Failed to load credentials and create gRPC connection to SA")
	sac := sapb.NewStorageAuthorityClient(saConn)

	var ctp *ctpolicy.CTPolicy
	conn, err := bgrpc.ClientSetup(c.RA.PublisherService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Failed to load credentials and create gRPC connection to Publisher")
	pubc := pubpb.NewPublisherClient(conn)

	apConn, err := bgrpc.ClientSetup(c.RA.AkamaiPurgerService, tlsConfig, clientMetrics, clk)
	cmd.FailOnError(err, "Unable to create a Akamai Purger client")
	apc := akamaipb.NewAkamaiPurgerClient(apConn)

	issuerCertPaths := c.RA.IssuerCerts
	if len(issuerCertPaths) == 0 {
		issuerCertPaths = []string{c.RA.IssuerCertPath}
	}
	issuerCerts := make([]*issuance.Certificate, len(issuerCertPaths))
	for i, issuerCertPath := range issuerCertPaths {
		issuerCerts[i], err = issuance.LoadCertificate(issuerCertPath)
		cmd.FailOnError(err, "Failed to load issuer certificate")
	}

	// Boulder's components assume that there will always be CT logs configured.
	// Issuing a certificate without SCTs embedded is a miss-issuance event in the
	// environment Boulder is built for. Exit early if there is no CTLogGroups2
	// configured.
	if len(c.RA.CTLogGroups2) == 0 {
		cmd.Fail("CTLogGroups2 must not be empty")
	}

	for i, g := range c.RA.CTLogGroups2 {
		// Exit early if any of the log groups specify no logs
		if len(g.Logs) == 0 {
			cmd.Fail(
				fmt.Sprintf("CTLogGroups2 index %d specifies no logs", i))
		}
		for _, l := range g.Logs {
			if l.TemporalSet != nil {
				err := l.Setup()
				cmd.FailOnError(err, "Failed to setup a temporal log set")
			}
		}
	}
	ctp = ctpolicy.New(pubc, c.RA.CTLogGroups2, c.RA.InformationalCTLogs, logger, scope)

	// TODO(patf): remove once RA.authorizationLifetimeDays is deployed
	authorizationLifetime := 300 * 24 * time.Hour
	if c.RA.AuthorizationLifetimeDays != 0 {
		authorizationLifetime = time.Duration(c.RA.AuthorizationLifetimeDays) * 24 * time.Hour
	}

	// TODO(patf): remove once RA.pendingAuthorizationLifetimeDays is deployed
	pendingAuthorizationLifetime := 7 * 24 * time.Hour
	if c.RA.PendingAuthorizationLifetimeDays != 0 {
		pendingAuthorizationLifetime = time.Duration(c.RA.PendingAuthorizationLifetimeDays) * 24 * time.Hour
	}

	// TODO(#5851): Remove these fallbacks when the old config keys are gone.
	if c.RA.GoodKey.WeakKeyFile == "" && c.RA.WeakKeyFile != "" {
		c.RA.GoodKey.WeakKeyFile = c.RA.WeakKeyFile
	}
	if c.RA.GoodKey.BlockedKeyFile == "" && c.RA.BlockedKeyFile != "" {
		c.RA.GoodKey.BlockedKeyFile = c.RA.BlockedKeyFile
	}
	kp, err := goodkey.NewKeyPolicy(&c.RA.GoodKey, sac.KeyBlocked)
	cmd.FailOnError(err, "Unable to create key policy")

	if c.RA.MaxNames == 0 {
		cmd.Fail("Error in RA config: MaxNames must not be 0")
	}

	rai := ra.NewRegistrationAuthorityImpl(
		clk,
		logger,
		scope,
		c.RA.MaxContactsPerRegistration,
		kp,
		c.RA.MaxNames,
		c.RA.ReuseValidAuthz,
		authorizationLifetime,
		pendingAuthorizationLifetime,
		pubc,
		caaClient,
		c.RA.OrderLifetime.Duration,
		ctp,
		apc,
		issuerCerts,
	)

	policyErr := rai.SetRateLimitPoliciesFile(c.RA.RateLimitPoliciesFilename)
	cmd.FailOnError(policyErr, "Couldn't load rate limit policies file")
	rai.PA = pa

	rai.VA = vac
	rai.CA = cac
	rai.SA = sac

	serverMetrics := bgrpc.NewServerMetrics(scope)
	grpcSrv, listener, err := bgrpc.NewServer(c.RA.GRPC, tlsConfig, serverMetrics, clk)
	cmd.FailOnError(err, "Unable to setup RA gRPC server")
	rapb.RegisterRegistrationAuthorityServer(grpcSrv, rai)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(grpcSrv, hs)

	go cmd.CatchSignals(logger, func() {
		hs.Shutdown()
		grpcSrv.GracefulStop()
	})

	err = cmd.FilterShutdownErrors(grpcSrv.Serve(listener))
	cmd.FailOnError(err, "RA gRPC service failed")
}

func init() {
	cmd.RegisterCommand("boulder-ra", main)
}
