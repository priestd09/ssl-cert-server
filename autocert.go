package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"io/ioutil"
	"log"
	mathrand "math/rand"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/ocsp"
)

// renewJitter is the maximum deviation from Manager.RenewBefore.
const renewJitter = time.Hour

// pseudoRand is safe for concurrent use.
var pseudoRand *lockedMathRand

// httpClient is used to do http request instead of the default http.DefaultClient.
// The OCSP server of Let's Encrypt certificates seems working improperly, gives
// `Unsolicited response received on idle HTTP channel starting with "HTTP/1.0 408 Request Time-out"`
// errors constantly after the service has been running for a long time.
// Using custom httpClient which disables Keep-Alive should fix this issue.
var httpClient *http.Client

func init() {
	pseudoRand = &lockedMathRand{rnd: mathrand.New(mathrand.NewSource(timeNow().UnixNano()))}
	httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				DualStack: true,
			}).DialContext,
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

var ErrHostNotPermitted = errors.New("host not permitted")

func HostWhitelist(hosts ...string) autocert.HostPolicy {
	whitelist := autocert.HostWhitelist(hosts...)
	return func(ctx context.Context, host string) error {
		if whitelist(ctx, host) != nil {
			return ErrHostNotPermitted
		}
		return nil
	}
}

func RegexpWhitelist(patterns ...*regexp.Regexp) autocert.HostPolicy {
	return func(_ context.Context, host string) error {
		for _, p := range patterns {
			if p.MatchString(host) {
				return nil
			}
		}
		return ErrHostNotPermitted
	}
}

func EncodeRSAKey(w io.Writer, key *rsa.PrivateKey) error {
	b := x509.MarshalPKCS1PrivateKey(key)
	pb := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: b}
	return pem.Encode(w, pb)
}

func EncodeECDSAKey(w io.Writer, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	pb := &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	return pem.Encode(w, pb)
}

type Manager struct {
	m        *autocert.Manager
	ForceRSA bool

	ocspStateMu sync.RWMutex
	ocspState   map[string]*ocspState
}

func (m *Manager) KeyName(domain string) string {
	if !m.ForceRSA {
		return domain
	}
	return domain + "+rsa"
}

func (m *Manager) helloInfo(domain string) *tls.ClientHelloInfo {
	helloInfo := &tls.ClientHelloInfo{ServerName: domain}
	if !m.ForceRSA {
		helloInfo.SignatureSchemes = append(helloInfo.SignatureSchemes,
			tls.ECDSAWithP256AndSHA256,
			tls.ECDSAWithP384AndSHA384,
			tls.ECDSAWithP521AndSHA512,
		)
		helloInfo.SupportedCurves = append(helloInfo.SupportedCurves, tls.CurveP256, tls.CurveP384, tls.CurveP521)
		helloInfo.CipherSuites = append(helloInfo.CipherSuites,
			tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		)
	}
	return helloInfo
}

func (m *Manager) GetCertificateByName(name string) (*tls.Certificate, error) {
	helloInfo := m.helloInfo(name)
	cert, err := m.m.GetCertificate(helloInfo)
	if err != nil {
		return nil, err
	}
	// trigger caching OCSP stapling in case of new certificate
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		m.ocspStateMu.Lock()
		defer m.ocspStateMu.Unlock()
		keyName := m.KeyName(name)
		_, err := m.loadOCSPState(ctx, name, cert)
		if err != nil {
			log.Println("trigger cache OCSP stapling failed: key_name=", keyName, "err=", err)
		}
	}()
	return cert, nil
}

func (m *Manager) GetOCSPStapling(domain string) ([]byte, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m.ocspStateMu.Lock()
	defer m.ocspStateMu.Unlock()
	keyName := m.KeyName(domain)
	if _, ok := m.ocspState[keyName]; !ok {
		if _, err := m.m.Cache.Get(ctx, keyName); err != nil {
			return nil, time.Time{}, err
		}
	}
	cert, err := m.m.GetCertificate(m.helloInfo(domain))
	if err != nil {
		return nil, time.Time{}, err
	}
	state, err := m.loadOCSPState(ctx, domain, cert)
	if err != nil {
		return nil, time.Time{}, err
	}
	return state.ocspDER, state.nextUpdate, nil
}

// loadOCSPState load OCSP stapling state for the given domain name and certificate
// from cache if available, or request new OCSP stapling from the certificate's
// OCSP server and cache the ocspState in Manager, caller must hold the lock ocspStateMu.
func (m *Manager) loadOCSPState(ctx context.Context, domain string, cert *tls.Certificate) (*ocspState, error) {
	keyName := m.KeyName(domain)
	state, ok := m.ocspState[keyName]
	if ok {
		if bytes.Equal(state.cert.Certificate[0], cert.Certificate[0]) {
			return state, nil
		}
		// the cached state is outdated, remove it
		state.renewal.stop()
		delete(m.ocspState, keyName)
	}

	issuer, err := x509.ParseCertificate(cert.Certificate[len(cert.Certificate)-1])
	if err != nil {
		return nil, err
	}
	der, response, err := m.requestOCSPStapling(ctx, cert, issuer)
	if err != nil {
		return nil, err
	}
	state = &ocspState{
		cert:       cert,
		issuer:     issuer,
		ocspDER:    der,
		nextUpdate: response.NextUpdate,
		renewal:    &ocspRenewal{m: m, domain: domain, keyName: keyName},
	}
	if m.ocspState == nil {
		m.ocspState = make(map[string]*ocspState)
	}
	m.ocspState[keyName] = state

	// start OCSP stapling renewal timer loop
	go state.renewal.start(state.nextUpdate)

	return state, nil
}

func (m *Manager) requestOCSPStapling(ctx context.Context, cert *tls.Certificate, issuer *x509.Certificate) (der []byte, resp *ocsp.Response, err error) {
	ocspReq, err := ocsp.CreateRequest(cert.Leaf, issuer, nil)
	if err != nil {
		return nil, nil, err
	}
	httpReq, err := http.NewRequest("POST", cert.Leaf.OCSPServer[0], bytes.NewBuffer(ocspReq))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/ocsp-request")
	httpResp, err := httpClient.Do(httpReq.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer httpResp.Body.Close()
	der, err = ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return nil, nil, err
	}
	resp, err = ocsp.ParseResponse(der, issuer)
	if err != nil {
		return nil, nil, err
	}
	return der, resp, nil
}

type ocspState struct {
	sync.RWMutex
	cert       *tls.Certificate
	issuer     *x509.Certificate
	ocspDER    []byte
	nextUpdate time.Time
	renewal    *ocspRenewal
}

type ocspRenewal struct {
	m       *Manager
	domain  string
	keyName string

	timerMu sync.Mutex
	timer   *time.Timer
}

func (or *ocspRenewal) start(next time.Time) {
	or.timerMu.Lock()
	defer or.timerMu.Unlock()
	if or.timer != nil {
		return
	}
	or.timer = time.AfterFunc(or.next(next), or.update)
	log.Println("started OCSP stapling renewal: key_name=", or.keyName, "next_update=", next.Format(time.RFC3339Nano))
}

func (or *ocspRenewal) stop() {
	or.timerMu.Lock()
	defer or.timerMu.Unlock()
	if or.timer == nil {
		return
	}
	or.timer.Stop()
	or.timer = nil
	log.Println("stoped OCSP stapling renewal: key_name=", or.keyName)
}

func (or *ocspRenewal) update() {
	or.timerMu.Lock()
	defer or.timerMu.Unlock()
	if or.timer == nil { // has been stopped
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	or.m.ocspStateMu.RLock()
	state, ok := or.m.ocspState[or.keyName]
	or.m.ocspStateMu.RUnlock()
	if !ok || state.renewal != or {
		// state has been removed / replaced, stop the old renewal
		or.timer = nil
		return
	}

	var next time.Duration
	der, response, err := or.m.requestOCSPStapling(ctx, state.cert, state.issuer)
	if err != nil {
		log.Println("update OCSP stapling failed: key_name=", or.keyName, "err=", err)
		next = renewJitter / 2
		next += time.Duration(pseudoRand.int63n(int64(next)))
	} else {
		log.Println("update OCSP stapling success: key_name=", or.keyName, "next_update=", response.NextUpdate.Format(time.RFC3339Nano))
		state.Lock()
		defer state.Unlock()
		state.ocspDER = der
		state.nextUpdate = response.NextUpdate
		next = or.next(response.NextUpdate)
	}

	or.timer = time.AfterFunc(next, or.update)
	testOCSPDidUpdateLoop(next, err)
}

func (or *ocspRenewal) next(expiry time.Time) time.Duration {
	var d time.Duration
	if expiry.Sub(timeNow()) > 48*time.Hour {
		d = expiry.Sub(timeNow()) - 48*time.Hour
	}
	// add a bit randomness to renew deadline
	n := pseudoRand.int63n(int64(renewJitter))
	d -= time.Duration(n)
	if d < 0 {
		// force sleep a while before next update
		n := pseudoRand.int63n(int64(time.Minute))
		d = time.Minute + time.Duration(n)
	}
	return d
}

type lockedMathRand struct {
	sync.Mutex
	rnd *mathrand.Rand
}

func (r *lockedMathRand) int63n(max int64) int64 {
	r.Lock()
	n := r.rnd.Int63n(max)
	r.Unlock()
	return n
}

var testOCSPDidUpdateLoop = func(next time.Duration, err error) {}

var timeNow = time.Now
