package kubeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hashicorp/go-cleanhttp"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	configPath = "config"
	rolePrefix = "role/"

	// aliasNameSourceUnset provides backwards compatibility with preexisting roles.
	aliasNameSourceUnset   = ""
	aliasNameSourceSAUid   = "serviceaccount_uid"
	aliasNameSourceSAName  = "serviceaccount_name"
	aliasNameSourceDefault = aliasNameSourceSAUid
)

var (
	// when adding new alias name sources make sure to update the corresponding FieldSchema description in path_role.go
	aliasNameSources          = []string{aliasNameSourceSAUid, aliasNameSourceSAName}
	errInvalidAliasNameSource = fmt.Errorf(`invalid alias_name_source, must be one of: %s`, strings.Join(aliasNameSources, ", "))

	// jwtReloadPeriod is the time period how often the in-memory copy of local
	// service account token can be used, before reading it again from disk.
	//
	// The value is selected according to recommendation in Kubernetes 1.21 changelog:
	// "Clients should reload the token from disk periodically (once per minute
	// is recommended) to ensure they continue to use a valid token."
	jwtReloadPeriod = 1 * time.Minute

	// caReloadPeriod is the time period how often the in-memory copy of local
	// CA cert can be used, before reading it again from disk.
	caReloadPeriod = 1 * time.Hour
)

// kubeAuthBackend implements logical.Backend
type kubeAuthBackend struct {
	*framework.Backend

	// default HTTP client for connection reuse
	httpClient *http.Client

	// reviewFactory is used to configure the strategy for doing a token review.
	// Currently the only options are using the kubernetes API or mocking the
	// review. Mocks should only be used in tests.
	reviewFactory tokenReviewFactory

	// localSATokenReader caches the service account token in memory.
	// It periodically reloads the token to support token rotation/renewal.
	// Local token is used when running in a pod with following configuration
	// - token_reviewer_jwt is not set
	// - disable_local_ca_jwt is false
	localSATokenReader *cachingFileReader

	// localCACertReader contains the local CA certificate. Local CA certificate is
	// used when running in a pod with following configuration
	// - kubernetes_ca_cert is not set
	// - disable_local_ca_jwt is false
	localCACertReader *cachingFileReader

	l sync.RWMutex
}

// Factory returns a new backend as logical.Backend.
func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := Backend()
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

func Backend() *kubeAuthBackend {
	b := &kubeAuthBackend{
		localSATokenReader: newCachingFileReader(localJWTPath, jwtReloadPeriod, time.Now),
		localCACertReader:  newCachingFileReader(localCACertPath, caReloadPeriod, time.Now),
	}

	b.Backend = &framework.Backend{
		AuthRenew:   b.pathLoginRenew(),
		BackendType: logical.TypeCredential,
		Help:        backendHelp,
		PathsSpecial: &logical.Paths{
			Unauthenticated: []string{
				"login",
			},
			SealWrapStorage: []string{
				configPath,
			},
		},
		Paths: framework.PathAppend(
			[]*framework.Path{
				pathConfig(b),
				pathLogin(b),
			},
			pathsRole(b),
		),
	}

	// Set a default http client.
	b.httpClient = cleanhttp.DefaultPooledClient()


	// Set the review factory to default to calling into the kubernetes API.
	b.reviewFactory = tokenReviewAPIFactory

	return b
}

// config takes a storage object and returns a kubeConfig object.
// It does not return local token and CA file which are specific to the pod we run in.
func (b *kubeAuthBackend) config(ctx context.Context, s logical.Storage) (*kubeConfig, error) {
	raw, err := s.Get(ctx, configPath)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}

	conf := &kubeConfig{}
	if err := json.Unmarshal(raw.Value, conf); err != nil {
		return nil, err
	}

	// Parse the public keys from the CertificatesBytes
	conf.PublicKeys = make([]interface{}, len(conf.PEMKeys))
	for i, cert := range conf.PEMKeys {
		conf.PublicKeys[i], err = parsePublicKeyPEM([]byte(cert))
		if err != nil {
			return nil, err
		}
	}

	return conf, nil
}

// loadConfig fetches the kubeConfig from storage and optionally decorates it with
// local token and CA certificate.
func (b *kubeAuthBackend) loadConfig(ctx context.Context, s logical.Storage) (*kubeConfig, error) {
	config, err := b.config(ctx, s)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("could not load backend configuration")
	}

	// Nothing more to do if loading local CA cert and JWT token is disabled.
	if config.DisableLocalCAJwt {
		return config, nil
	}

	// Read local JWT token unless it was not stored in config.
	if config.TokenReviewerJWT == "" {
		config.TokenReviewerJWT, err = b.localSATokenReader.ReadFile()
		if err != nil {
			// Ignore error: make best effort trying to load local JWT,
			// otherwise the JWT submitted in login payload will be used.
			b.Logger().Debug("failed to read local service account token, will use client token", "error", err)
		}
	}

	// Read local CA cert unless it was stored in config.
	if config.CACert == "" {
		config.CACert, err = b.localCACertReader.ReadFile()
		if err != nil {
			return nil, err
		}
	}

	return config, nil
}

// role takes a storage backend and the name and returns the role's storage
// entry
func (b *kubeAuthBackend) role(ctx context.Context, s logical.Storage, name string) (*roleStorageEntry, error) {
	raw, err := s.Get(ctx, fmt.Sprintf("%s%s", rolePrefix, strings.ToLower(name)))
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}

	role := &roleStorageEntry{}
	if err := json.Unmarshal(raw.Value, role); err != nil {
		return nil, err
	}

	if role.TokenTTL == 0 && role.TTL > 0 {
		role.TokenTTL = role.TTL
	}
	if role.TokenMaxTTL == 0 && role.MaxTTL > 0 {
		role.TokenMaxTTL = role.MaxTTL
	}
	if role.TokenPeriod == 0 && role.Period > 0 {
		role.TokenPeriod = role.Period
	}
	if role.TokenNumUses == 0 && role.NumUses > 0 {
		role.TokenNumUses = role.NumUses
	}
	if len(role.TokenPolicies) == 0 && len(role.Policies) > 0 {
		role.TokenPolicies = role.Policies
	}
	if len(role.TokenBoundCIDRs) == 0 && len(role.BoundCIDRs) > 0 {
		role.TokenBoundCIDRs = role.BoundCIDRs
	}

	return role, nil
}

func validateAliasNameSource(source string) error {
	for _, s := range aliasNameSources {
		if s == source {
			return nil
		}
	}
	return errInvalidAliasNameSource
}

var backendHelp string = `
The Kubernetes Auth Backend allows authentication for Kubernetes service accounts.
`
