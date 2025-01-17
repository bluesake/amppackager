// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certcache

import (
	"crypto/x509"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/WICG/webpackage/go/signedexchange/cbor"
	"github.com/ampproject/amppackager/packager/mux"
	pkgt "github.com/ampproject/amppackager/packager/testing"
	"github.com/ampproject/amppackager/packager/util"
	"github.com/stretchr/testify/suite"
	ocsptest "github.com/twifkak/crypto/ocsp"
	"golang.org/x/crypto/ocsp"
)

// FakeOCSPResponse returns a DER-encoded fake OCSP response. producedAt is
// rounded up to the nearest minute, rather than the default ocsp lib behavior
// of rounding down, so that calls to this function with producedAt ==
// thisUpdate return a valid response.
func FakeOCSPResponse(thisUpdate, producedAt time.Time) ([]byte, error) {
	template := ocsptest.Response{
		Status:           ocsp.Good,
		SerialNumber:     pkgt.B3Certs[0].SerialNumber,
		ThisUpdate:       thisUpdate,
		NextUpdate:       thisUpdate.Add(7 * 24 * time.Hour),
		RevokedAt:        thisUpdate.AddDate( /*years=*/ 0 /*months=*/, 0 /*days=*/, 365),
		RevocationReason: ocsp.Unspecified,
	}
	return ocsptest.CreateResponse(pkgt.CACert, pkgt.CACert, template, pkgt.CAKey, producedAt.Add(1*time.Minute))
}

type CertCacheSuite struct {
	suite.Suite
	fakeOCSP            []byte
	fakeOCSPExpiry      *time.Time
	ocspServer          *httptest.Server // "const", do not set
	ocspServerWasCalled bool
	ocspHandler         func(w http.ResponseWriter, req *http.Request)
	tempDir             string
	handler             *CertCache
	fakeClock           *pkgt.FakeClock
}

func stringPtr(s string) *string {
	return &s
}

func (this *CertCacheSuite) New() (*CertCache, error) {
	// TODO(twifkak): Stop the old CertCache's goroutine.
	// TODO(banaag): Consider adding a test with certfetcher set.
	//  For now, this tests certcache without worrying about certfetcher.
	// certCache := New(pkgt.B3Certs, nil, []string{"example.com"}, "cert.crt", "newcert.crt",
	// 	filepath.Join(this.tempDir, "ocsp"), nil, time.Now)
	certCache := New(pkgt.B3Certs, nil, []string{"example.com"}, "cert.crt", "newcert.crt",
		filepath.Join(this.tempDir, "ocsp"), nil, this.fakeClock.Now)
	certCache.extractOCSPServer = func(*x509.Certificate) (string, error) {
		return this.ocspServer.URL, nil
	}
	defaultHttpExpiry := certCache.httpExpiry
	certCache.httpExpiry = func(req *http.Request, resp *http.Response) time.Time {
		if this.fakeOCSPExpiry != nil {
			return *this.fakeOCSPExpiry
		} else {
			return defaultHttpExpiry(req, resp)
		}
	}
	err := certCache.Init()
	return certCache, err
}

func (this *CertCacheSuite) SetupSuite() {
	this.ocspServer = httptest.NewServer(http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		this.ocspHandler(resp, req)
	}))
}

func (this *CertCacheSuite) TearDownSuite() {
	this.ocspServer.Close()
}

func (this *CertCacheSuite) SetupTest() {
	// Set fake clock to a time 8 days past the NotBefore of the cert, so
	// our tests can backdate OCSPs to test expiry logic, without
	// accidentally hitting the requirement that OCSPs must postdate certs.
	this.fakeClock = pkgt.NewFakeClock()
	this.fakeClock.SecondsSince0 = pkgt.B3Certs[0].NotBefore.Add(8 * 24 * time.Hour).Sub(time.Unix(0, 0))
	now := this.fakeClock.Now()
	var err error
	this.fakeOCSP, err = FakeOCSPResponse(now, now)
	this.Require().NoError(err, "creating fake OCSP response")

	this.ocspHandler = func(resp http.ResponseWriter, req *http.Request) {
		this.ocspServerWasCalled = true
		_, err := resp.Write(this.fakeOCSP)
		this.Require().NoError(err, "writing fake OCSP response")
	}

	this.tempDir, err = ioutil.TempDir(os.TempDir(), "certcache_test")
	this.Require().NoError(err, "setting up test harness")

	this.handler, err = this.New()
	this.Require().NoError(err, "instantiating CertCache")
}

func (this *CertCacheSuite) TearDownTest() {
	// Reset any variables that may have been overridden in test and won't be rewritten in SetupTest.
	this.fakeOCSPExpiry = nil

	// Reverse SetupTest.
	this.handler.Stop()

	err := os.RemoveAll(this.tempDir)
	if err != nil {
		log.Panic("Error removing temp dir", err)
	}
}

func (this *CertCacheSuite) mux() http.Handler {
	return mux.New(this.handler, nil, nil, nil, nil)
}

func (this *CertCacheSuite) ocspServerCalled(f func()) bool {
	this.ocspServerWasCalled = false
	f()
	return this.ocspServerWasCalled
}

func (this *CertCacheSuite) DecodeCBOR(r io.Reader) map[string][]byte {
	decoder := cbor.NewDecoder(r)

	// Our test cert chain has exactly two certs. First entry is a magic.
	numItems, err := decoder.DecodeArrayHeader()
	this.Require().NoError(err, "decoding array header")
	this.Require().EqualValues(3, numItems)

	magic, err := decoder.DecodeTextString()
	this.Require().NoError(err, "decoding magic")
	this.Require().Equal("📜⛓", magic)

	// Decode and return the first one.
	numKeys, err := decoder.DecodeMapHeader()
	this.Require().NoError(err, "decoding map header")
	this.Require().EqualValues(2, numKeys)

	ret := map[string][]byte{}
	for i := 0; uint64(i) < numKeys; i++ {
		key, err := decoder.DecodeTextString()
		this.Require().NoError(err, "decoding key")
		value, err := decoder.DecodeByteString()
		this.Require().NoError(err, "decoding value")
		ret[key] = value
	}
	return ret
}

func (this *CertCacheSuite) TestServesCertificate() {
	resp := pkgt.NewRequest(this.T(), this.mux(), "/amppkg/cert/"+pkgt.CertName).Do()
	this.Assert().Equal(http.StatusOK, resp.StatusCode, "incorrect status: %#v", resp)
	this.Assert().Equal("nosniff", resp.Header.Get("X-Content-Type-Options"))
	cbor := this.DecodeCBOR(resp.Body)
	this.Assert().Contains(cbor, "cert")
	this.Assert().Contains(cbor, "ocsp")
	this.Assert().NotContains(cbor, "sct")
}

func (this *CertCacheSuite) TestCertCacheIsHealthy() {
	this.Assert().NoError(this.handler.IsHealthy())
}

func (this *CertCacheSuite) TestOCSPInvalidThisUpdate() {
	// Set fake clock equal to cert NotBefore, so we can produce an OCSP
	// where "now" is within its ThisUpdate/NextUpdate window, but the OCSP
	// is itself outside of the cert's NotBefore/NotAfter window.
	this.fakeClock.SecondsSince0 = pkgt.B3Certs[0].NotBefore.Sub(time.Unix(0, 0))

	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")

	// Build an OCSP response that's not expired, but invalid because it predates the cert:
	invalidOCSP, err := FakeOCSPResponse(this.fakeClock.Now().Add(-1*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating invalid OCSP response")
	this.fakeOCSP = invalidOCSP
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().EqualError(err, "initializing CertCache: Missing OCSP response.")
	}))
}

func (this *CertCacheSuite) TestCertCacheIsNotHealthy() {
	// Prime memory cache with a past-midpoint OCSP:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	this.fakeOCSP, err = FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating stale OCSP response")
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))

	// Prime disk cache with a bad OCSP:
	freshOCSP := []byte("0xdeadbeef")
	this.fakeOCSP = freshOCSP
	err = ioutil.WriteFile(filepath.Join(this.tempDir, "ocsp"), freshOCSP, 0644)
	this.Require().NoError(err, "writing fresh OCSP response to disk")

	this.Assert().True(this.ocspServerCalled(func() {
		this.handler.readOCSP(true)
	}))

	this.Assert().Error(this.handler.IsHealthy())
}

func (this *CertCacheSuite) TestServes404OnMissingCertificate() {
	resp := pkgt.NewRequest(this.T(), this.mux(), "/amppkg/cert/lalala").Do()
	this.Assert().Equal(http.StatusNotFound, resp.StatusCode, "incorrect status: %#v", resp)
	body, _ := ioutil.ReadAll(resp.Body)
	// Small enough not to fit a cert or key:
	this.Assert().Condition(func() bool { return len(body) <= 20 }, "body too large: %q", body)
}

func (this *CertCacheSuite) TestOCSP() {
	// Verify it gets included in the cert-chain+cbor payload.
	resp := pkgt.NewRequest(this.T(), this.mux(), "/amppkg/cert/"+pkgt.CertName).Do()
	this.Assert().Equal(http.StatusOK, resp.StatusCode, "incorrect status: %#v", resp)
	// 302400 is 3.5 days. max-age is slightly less because of the time between fake OCSP generation and cert-chain response.
	this.Assert().Equal("public, max-age=302388", resp.Header.Get("Cache-Control"))
	cbor := this.DecodeCBOR(resp.Body)
	this.Assert().Equal(this.fakeOCSP, cbor["ocsp"])
}

func (this *CertCacheSuite) TestOCSPCached() {
	// Verify it is in the memory cache:
	this.Assert().False(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Assert().NoError(err)
	}))

	// Create a new handler, to see it populates the memory cache from disk, not network:
	this.Assert().False(this.ocspServerCalled(func() {
		_, err := this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))
}

func (this *CertCacheSuite) TestOCSPExpiry() {
	// Prime memory and disk cache with a past-midpoint OCSP:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	this.fakeOCSP, err = FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating expired OCSP response")
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))

	// Verify HTTP response expires immediately:
	resp := pkgt.NewRequest(this.T(), this.mux(), "/amppkg/cert/"+pkgt.CertName).Do()
	this.Assert().Equal("public, max-age=0", resp.Header.Get("Cache-Control"))

	// On update, verify network is called:
	this.Assert().True(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Assert().NoError(err)
	}))
}

func (this *CertCacheSuite) TestOCSPUpdateFromDisk() {
	// Prime memory cache with a past-midpoint OCSP:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	this.fakeOCSP, err = FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating stale OCSP response")
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))

	// Prime disk cache with a fresh OCSP.
	now := this.fakeClock.Now()
	freshOCSP, err := FakeOCSPResponse(now, now)
	this.Require().NoError(err, "creating fresh OCSP response")
	err = ioutil.WriteFile(filepath.Join(this.tempDir, "ocsp"), freshOCSP, 0644)
	this.Require().NoError(err, "writing fresh OCSP response to disk")

	// On update, verify network is not called (fresh OCSP from disk is used):
	this.Assert().False(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Assert().NoError(err)
	}))
}

func (this *CertCacheSuite) TestOCSPExpiredViaHTTPHeaders() {
	// Prime memory and disk cache with a fresh OCSP but soon-to-expire HTTP headers:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	this.fakeOCSPExpiry = new(time.Time)
	*this.fakeOCSPExpiry = time.Unix(0, 1) // Infinite past. time.Time{} is used as a sentinel value to mean no update.
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinitializing CertCache")
	}))
	this.Require().Equal(time.Unix(0, 1), this.handler.ocspUpdateAfter)

	// Verify that, 2 seconds later, a new fetch is attempted.
	this.Assert().True(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Require().NoError(err, "updating OCSP")
	}))
}

func (this *CertCacheSuite) TestOCSPIgnoreExpiredNextUpdate() {
	// Prime memory and disk cache with a past-midpoint OCSP:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	staleOCSP, err := FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating stale OCSP response")
	this.fakeOCSP = staleOCSP
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))

	// Try to update with an invalid OCSP:
	this.fakeOCSP, err = FakeOCSPResponse(this.fakeClock.Now().Add(-8*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating expired OCSP response")
	this.Assert().True(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Require().NoError(err, "updating OCSP")
	}))

	// Verify that the invalid update doesn't squash the valid cache entry.
	ocsp, _, err := this.handler.readOCSP(true)
	this.Require().NoError(err, "reading OCSP")
	this.Assert().Equal(staleOCSP, ocsp)
}

func (this *CertCacheSuite) TestOCSPIgnoreInvalidProducedAt() {
	// Prime memory and disk cache with a past-midpoint OCSP:
	err := os.Remove(filepath.Join(this.tempDir, "ocsp"))
	this.Require().NoError(err, "deleting OCSP tempfile")
	staleOCSP, err := FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), this.fakeClock.Now())
	this.Require().NoError(err, "creating stale OCSP response")
	this.fakeOCSP = staleOCSP
	this.Require().True(this.ocspServerCalled(func() {
		this.handler, err = this.New()
		this.Require().NoError(err, "reinstantiating CertCache")
	}))

	// Try to update with an OCSP with an invalid ProducedAt:
	this.fakeOCSP, err = FakeOCSPResponse(this.fakeClock.Now().Add(-4*24*time.Hour), time.Unix(0, 0))
	this.Require().NoError(err, "creating invalid OCSP response")
	this.Assert().True(this.ocspServerCalled(func() {
		_, _, err := this.handler.readOCSP(true)
		this.Require().NoError(err, "updating OCSP")
	}))

	// Verify that the invalid update doesn't squash the valid cache entry.
	ocsp, _, err := this.handler.readOCSP(true)
	this.Require().NoError(err, "reading OCSP")
	this.Assert().Equal(staleOCSP, ocsp)
}

func (this *CertCacheSuite) TestPopulateCertCache() {
	certCache, err := PopulateCertCache(
		&util.Config{
			CertFile:    "../../testdata/b3/fullchain.cert",
			NewCertFile: "/tmp/newcert.cert",
			KeyFile:     "../../testdata/b3/server.privkey",
			OCSPCache:   "/tmp/ocsp",
			URLSet: []util.URLSet{{
				Sign: &util.URLPattern{
					Domain:    "amppackageexample.com",
					PathRE:    stringPtr(".*"),
					QueryRE:   stringPtr(""),
					MaxLength: 2000,
				},
			}},
		},
		pkgt.B3Key,
		nil,
		true,
		false)
	this.Require().NoError(err)
	this.Assert().NotNil(certCache)
	this.Assert().Equal(pkgt.B3Certs[0], certCache.GetLatestCert())
}

func TestCertCacheSuite(t *testing.T) {
	suite.Run(t, new(CertCacheSuite))
}
