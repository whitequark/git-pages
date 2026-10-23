package git_pages

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// This type is a more or less a copy of golang/x/crypto/acme/autocert/listener.go, except
// that unlike the standard library one, it is possible to use it with a non-default host/port.
// (You need a custom type here in order for the listener to respond to tls-alpn-01 challenges.)
// It's also been modified to be generic over whether TLS is enabled.
type tlsListener struct {
	net.Listener
	tlsConfig *tls.Config
}

func (acmeListener *tlsListener) Accept() (net.Conn, error) {
	conn, err := acmeListener.Listener.Accept()
	if err != nil {
		return nil, err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		// Because Listener is a convenience function, help out with
		// this too.  This is not possible for the caller to set once
		// we return a *tcp.Conn wrapping an inaccessible net.Conn.
		// If callers don't want this, they can do things the manual
		// way and tweak as needed. But this is what net/http does
		// itself, so copy that. If net/http changes, we can change
		// here too.
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(3 * time.Minute)

		if acmeListener.tlsConfig != nil {
			return tls.Server(tcpConn, acmeListener.tlsConfig), nil
		} else {
			return tcpConn, nil
		}
	} else {
		return conn, nil
	}
}

const autocertNamespace = "autocert:"

type secretAcmeCache struct{}

func (secretAcmeCache) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := secrets.GetACME(ctx, autocertNamespace+key)
	if errors.Is(err, ErrObjectNotFound) {
		err = autocert.ErrCacheMiss
	}
	return data, err
}

func (secretAcmeCache) Put(ctx context.Context, key string, data []byte) error {
	return secrets.PutACME(ctx, autocertNamespace+key, data)
}

func (secretAcmeCache) Delete(ctx context.Context, key string) error {
	return secrets.DeleteACME(ctx, autocertNamespace+key)
}

var errDomainNotServed = errors.New("domain not served")

func ACMEConfig(ctx context.Context) *tls.Config {
	if config.Acme.Directory == "" {
		logc.Fatalln(ctx, "acme: directory not specified")
		return nil
	}

	if config.Acme.Email == "" {
		logc.Fatalln(ctx, "acme: email not specified")
		return nil
	}

	if !config.Acme.AcceptTOS {
		logc.Fatalln(ctx, "acme: must accept terms of service")
		return nil
	}

	acmeManager := &autocert.Manager{
		Client: &acme.Client{
			DirectoryURL: config.Acme.Directory,
		},
		Email:  config.Acme.Email,
		Cache:  secretAcmeCache{},
		Prompt: func(tosURL string) bool { return config.Acme.AcceptTOS },
		HostPolicy: func(ctx context.Context, domain string) error {
			if existenceCache.CheckDomain(ctx, domain).IsPossible() {
				if found, err := backend.CheckDomain(ctx, domain); found {
					return nil
				} else if err != nil {
					return err
				}
			}
			return errDomainNotServed
		},
	}
	return acmeManager.TLSConfig()
}

func ACMEClient(ctx context.Context) (*acme.Client, error) {
	// The documentation for autocert clearly says not to rely on any specific filenames that
	// it saves in the cache. But it also doesn't provide any way to get an acme.Client out of
	// autocert.Manager, nor does it offer a way to get the account ID. We'll need this for
	// dns-persist-01 ACME challenge, so something has to give here, and reading the key directly
	// seems like the least bad option. (The second best option is to copy the entirety of autocert
	// and modify it to expose the client, which seems like a terrible solution.)
	//
	// Go 1.26+ will always create ECDSA keys, so we only support this format and key file name.
	const keyName = "acme_account+key"

	privKeyPEM, err := secrets.GetACME(ctx, keyName)
	if err != nil {
		return nil, err
	}

	privKeyBlock, _ := pem.Decode(privKeyPEM)
	if privKeyBlock == nil {
		return nil, errors.New("invalid ACME account key")
	}

	privKey, err := x509.ParseECPrivateKey(privKeyBlock.Bytes)
	if err != nil {
		return nil, err
	}

	return &acme.Client{
		DirectoryURL: config.Acme.Directory,
		UserAgent:    "git-pages",
		Key:          privKey,
	}, nil
}
