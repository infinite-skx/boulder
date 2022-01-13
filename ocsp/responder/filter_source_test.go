package responder

import (
	"context"
	"crypto"
	"encoding/hex"
	"io/ioutil"
	"testing"

	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/issuance"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/metrics"
	"github.com/letsencrypt/boulder/test"
	"golang.org/x/crypto/ocsp"
)

func TestNewFilter(t *testing.T) {
	_, err := NewFilterSource([]*issuance.Certificate{}, []string{}, nil, metrics.NoopRegisterer, blog.NewMock())
	test.AssertError(t, err, "Didn't error when creating empty filter")

	issuer, err := issuance.LoadCertificate("./testdata/test-ca.der.pem")
	test.AssertNotError(t, err, "Failed to load issuer cert")
	issuerNameId := issuer.NameID()

	f, err := NewFilterSource([]*issuance.Certificate{issuer}, []string{"00"}, nil, metrics.NoopRegisterer, blog.NewMock())
	test.AssertNotError(t, err, "Errored when creating good filter")
	test.AssertEquals(t, len(f.issuers), 1)
	test.AssertEquals(t, len(f.serialPrefixes), 1)
	test.AssertEquals(t, hex.EncodeToString(f.issuers[issuerNameId].keyHash), "fb784f12f96015832c9f177f3419b32e36ea4189")
}

func TestCheckRequest(t *testing.T) {
	issuer, err := issuance.LoadCertificate("./testdata/test-ca.der.pem")
	test.AssertNotError(t, err, "Failed to load issuer cert")

	f, err := NewFilterSource([]*issuance.Certificate{issuer}, []string{"00"}, nil, metrics.NoopRegisterer, blog.NewMock())
	test.AssertNotError(t, err, "Errored when creating good filter")

	reqBytes, err := ioutil.ReadFile("./testdata/ocsp.req")
	test.AssertNotError(t, err, "failed to read OCSP request")

	// Select a bad hash algorithm.
	ocspReq, err := ocsp.ParseRequest(reqBytes)
	test.AssertNotError(t, err, "Failed to prepare fake ocsp request")
	ocspReq.HashAlgorithm = crypto.MD5
	_, err = f.Response(context.Background(), ocspReq)
	test.AssertError(t, err, "Accepted ocsp request with bad hash algorithm")

	// Make the hash invalid.
	ocspReq, err = ocsp.ParseRequest(reqBytes)
	test.AssertNotError(t, err, "Failed to prepare fake ocsp request")
	ocspReq.IssuerKeyHash[0]++
	_, err = f.Response(context.Background(), ocspReq)
	test.AssertError(t, err, "Accepted ocsp request with bad issuer key hash")

	// Make the serial prefix wrong by incrementing the first byte by 1.
	ocspReq, err = ocsp.ParseRequest(reqBytes)
	test.AssertNotError(t, err, "Failed to prepare fake ocsp request")
	serialStr := []byte(core.SerialToString(ocspReq.SerialNumber))
	serialStr[0] = serialStr[0] + 1
	ocspReq.SerialNumber.SetString(string(serialStr), 16)
	_, err = f.Response(context.Background(), ocspReq)
	test.AssertError(t, err, "Accepted ocsp request with bad serial prefix")
}

type echoSource struct {
	resp *Response
}

func (src *echoSource) Response(context.Context, *ocsp.Request) (*Response, error) {
	return src.resp, nil
}

func TestCheckResponse(t *testing.T) {
	issuer, err := issuance.LoadCertificate("./testdata/test-ca.der.pem")
	test.AssertNotError(t, err, "failed to load issuer cert")

	reqBytes, err := ioutil.ReadFile("./testdata/ocsp.req")
	test.AssertNotError(t, err, "failed to read OCSP request")
	req, err := ocsp.ParseRequest(reqBytes)
	test.AssertNotError(t, err, "failed to prepare fake ocsp request")

	respBytes, err := ioutil.ReadFile("./testdata/ocsp.resp")
	test.AssertNotError(t, err, "failed to read OCSP response")
	resp, err := ocsp.ParseResponse(respBytes, nil)
	test.AssertNotError(t, err, "failed to parse OCSP response")

	source := &echoSource{&Response{resp, respBytes}}
	f, err := NewFilterSource([]*issuance.Certificate{issuer}, []string{"00"}, source, metrics.NoopRegisterer, blog.NewMock())
	test.AssertNotError(t, err, "errored when creating good filter")

	actual, err := f.Response(context.Background(), req)
	test.AssertNotError(t, err, "unexpected error")
	test.AssertEquals(t, actual.Response, resp)

	// Overwrite the Responder Name in the stored response to cause a diagreement.
	resp.RawResponderName = []byte("C = US, O = Foo, DN = Bar")
	source = &echoSource{&Response{resp, respBytes}}
	f, err = NewFilterSource([]*issuance.Certificate{issuer}, []string{"00"}, source, metrics.NoopRegisterer, blog.NewMock())
	test.AssertNotError(t, err, "errored when creating good filter")

	_, err = f.Response(context.Background(), req)
	test.AssertError(t, err, "expected error")
}
