// Package google implements logging in through Google's OpenID Connect provider.
package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"

	"github.com/dexidp/dex/connector"
	pkg_groups "github.com/dexidp/dex/pkg/groups"
	"github.com/dexidp/dex/pkg/log"
)

const (
	issuerURL = "https://accounts.google.com"
)

// Config holds configuration options for Google logins.
type Config struct {
	ClientID     string `json:"clientID"`
	ClientSecret string `json:"clientSecret"`
	RedirectURI  string `json:"redirectURI"`

	Scopes []string `json:"scopes"` // defaults to "profile" and "email"

	// Optional list of whitelisted domains
	// If this field is nonempty, only users from a listed domain will be allowed to log in
	HostedDomains []string `json:"hostedDomains"`

	// Optional list of whitelisted groups
	// If this field is nonempty, only users from a listed group will be allowed to log in
	Groups []string `json:"groups"`

	// Optional path to service account json
	// If nonempty, and groups claim is made, will use authentication from file to
	// check groups with the admin directory api
	ServiceAccountFilePath string `json:"serviceAccountFilePath"`

	// Required if ServiceAccountFilePath
	// The email of a GSuite super user which the service account will impersonate
	// when listing groups
	AdminEmail string

	// If this field is true, fetch direct group membership and transitive group membership
	FetchTransitiveGroupMembership bool `json:"fetchTransitiveGroupMembership"`

	// If this field is true, fetch groups with the Google Directory API
	FetchGroupsWithDirectoryService bool `json:"fetchGroupsWithDirectoryService"`

	// Domain is the domain to fetch groups from
	Domain string `json:"domain"`
}

// Open returns a connector which can be used to login users through Google.
func (c *Config) Open(id string, logger log.Logger) (conn connector.Connector, err error) {
	ctx, cancel := context.WithCancel(context.Background())

	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to get provider: %v", err)
	}

	scopes := []string{oidc.ScopeOpenID}
	if len(c.Scopes) > 0 {
		scopes = append(scopes, c.Scopes...)
	} else {
		scopes = append(scopes, "profile", "email")
	}

	var adminSrv *admin.Service

	// Fixing a regression caused by default config fallback: https://github.com/dexidp/dex/issues/2699
	if c.FetchGroupsWithDirectoryService {
		srv, err := createDirectoryService(c.ServiceAccountFilePath, c.AdminEmail, logger)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("could not create directory service: %v", err)
		}

		adminSrv = srv
	}

	clientID := c.ClientID
	return &googleConnector{
		redirectURI: c.RedirectURI,
		oauth2Config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: c.ClientSecret,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
			RedirectURL:  c.RedirectURI,
		},
		verifier: provider.Verifier(
			&oidc.Config{ClientID: clientID},
		),
		logger:                         logger,
		cancel:                         cancel,
		hostedDomains:                  c.HostedDomains,
		groups:                         c.Groups,
		serviceAccountFilePath:         c.ServiceAccountFilePath,
		adminEmail:                     c.AdminEmail,
		domain:                         c.Domain,
		fetchTransitiveGroupMembership: c.FetchTransitiveGroupMembership,
		adminSrv:                       adminSrv,
	}, nil
}

var (
	_ connector.CallbackConnector = (*googleConnector)(nil)
	_ connector.RefreshConnector  = (*googleConnector)(nil)
)

type googleConnector struct {
	redirectURI                    string
	oauth2Config                   *oauth2.Config
	verifier                       *oidc.IDTokenVerifier
	cancel                         context.CancelFunc
	logger                         log.Logger
	hostedDomains                  []string
	domain                         string
	groups                         []string
	serviceAccountFilePath         string
	adminEmail                     string
	fetchTransitiveGroupMembership bool
	adminSrv                       *admin.Service
}

func (c *googleConnector) Close() error {
	c.cancel()
	return nil
}

func (c *googleConnector) LoginURL(s connector.Scopes, callbackURL, state string) (string, error) {
	if c.redirectURI != callbackURL {
		return "", fmt.Errorf("expected callback URL %q did not match the URL in the config %q", callbackURL, c.redirectURI)
	}

	var opts []oauth2.AuthCodeOption
	if len(c.hostedDomains) > 0 {
		preferredDomain := c.hostedDomains[0]
		if len(c.hostedDomains) > 1 {
			preferredDomain = "*"
		}
		opts = append(opts, oauth2.SetAuthURLParam("hd", preferredDomain))
	}

	if s.OfflineAccess {
		opts = append(opts, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	return c.oauth2Config.AuthCodeURL(state, opts...), nil
}

type oauth2Error struct {
	error            string
	errorDescription string
}

func (e *oauth2Error) Error() string {
	if e.errorDescription == "" {
		return e.error
	}
	return e.error + ": " + e.errorDescription
}

func (c *googleConnector) HandleCallback(s connector.Scopes, r *http.Request) (identity connector.Identity, err error) {
	q := r.URL.Query()
	if errType := q.Get("error"); errType != "" {
		return identity, &oauth2Error{errType, q.Get("error_description")}
	}
	token, err := c.oauth2Config.Exchange(r.Context(), q.Get("code"))
	if err != nil {
		return identity, fmt.Errorf("google: failed to get token: %v", err)
	}

	return c.createIdentity(r.Context(), identity, s, token)
}

func (c *googleConnector) Refresh(ctx context.Context, s connector.Scopes, identity connector.Identity) (connector.Identity, error) {
	t := &oauth2.Token{
		RefreshToken: string(identity.ConnectorData),
		Expiry:       time.Now().Add(-time.Hour),
	}
	token, err := c.oauth2Config.TokenSource(ctx, t).Token()
	if err != nil {
		return identity, fmt.Errorf("google: failed to get token: %v", err)
	}

	return c.createIdentity(ctx, identity, s, token)
}

func (c *googleConnector) createIdentity(ctx context.Context, identity connector.Identity, s connector.Scopes, token *oauth2.Token) (connector.Identity, error) {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return identity, errors.New("google: no id_token in token response")
	}
	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return identity, fmt.Errorf("google: failed to verify ID Token: %v", err)
	}

	var claims struct {
		Username      string `json:"name"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		HostedDomain  string `json:"hd"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return identity, fmt.Errorf("oidc: failed to decode claims: %v", err)
	}

	if len(c.hostedDomains) > 0 {
		found := false
		for _, domain := range c.hostedDomains {
			if claims.HostedDomain == domain {
				found = true
				break
			}
		}

		if !found {
			return identity, fmt.Errorf("oidc: unexpected hd claim %v", claims.HostedDomain)
		}
	}

	var groups []string
	if s.Groups && c.adminSrv != nil {
		if c.fetchTransitiveGroupMembership {
			groups, err = c.getAllGroups(ctx, claims.Email)
		} else {
			groups, err = c.getGroups(ctx, claims.Email, &sync.Map{})
		}
		if err != nil {
			return identity, fmt.Errorf("google: could not retrieve groups: %v", err)
		}

		if len(c.groups) > 0 {
			groups = pkg_groups.Filter(groups, c.groups)
			if len(groups) == 0 {
				return identity, fmt.Errorf("google: user %q is not in any of the required groups", claims.Username)
			}
		}
	}

	identity = connector.Identity{
		UserID:        idToken.Subject,
		Username:      claims.Username,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		ConnectorData: []byte(token.RefreshToken),
		Groups:        groups,
	}
	return identity, nil
}

func (c *googleConnector) getAllGroups(ctx context.Context, userKey string) ([]string, error) {
	parentGroups, err := c.adminSrv.Groups.List().
		UserKey(userKey).
		Domain(c.domain).
		Context(ctx).
		Do()
	if err != nil {
		return nil, err
	}

	var groups []string
	var groupsCh = make(chan string)
	checkedGroups := sync.Map{}
	g, cctx := errgroup.WithContext(ctx)

	for _, group := range parentGroups.Groups {
		email := group.Email
		g.Go(func() error {
			childGroups, err := c.getGroups(cctx, email, &checkedGroups)
			if err != nil {
				return err
			}

			childGroups = append(childGroups, email)

			for _, email := range childGroups {
				select {
				case groupsCh <- email:
				case <-cctx.Done():
					return cctx.Err()
				}
			}

			return nil
		})
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-cctx.Done():
				close(done)
				return
			case g := <-groupsCh:
				groups = append(groups, g)
			}
		}
	}()

	if err := g.Wait(); err != nil {
		return nil, err
	}
	<-done

	return groups, nil
}

// getGroups creates a connection to the admin directory service and lists
// all groups the user is a member of
func (c *googleConnector) getGroups(ctx context.Context, email string, checkedGroups *sync.Map) ([]string, error) {
	var userGroups []string
	var err error
	groupsList := &admin.Groups{}
	for {
		groupsList, err = c.adminSrv.Groups.List().
			Domain(c.domain).
			UserKey(email).
			PageToken(groupsList.NextPageToken).
			Context(ctx).
			Do()
		if err != nil {
			return nil, fmt.Errorf("could not list groups: %v", err)
		}

		for _, group := range groupsList.Groups {
			if _, ok := checkedGroups.LoadOrStore(group.Email, struct{}{}); ok {
				continue
			}

			// TODO (joelspeed): Make desired group key configurable
			userGroups = append(userGroups, group.Email)

			if !c.fetchTransitiveGroupMembership {
				continue
			}

			// getGroups takes a user's email/alias as well as a group's email/alias
			transitiveGroups, err := c.getGroups(ctx, group.Email, checkedGroups)
			if err != nil {
				return nil, fmt.Errorf("could not list transitive groups: %v", err)
			}

			userGroups = append(userGroups, transitiveGroups...)
		}

		if groupsList.NextPageToken == "" {
			break
		}
	}

	return userGroups, nil
}

// createDirectoryService sets up super user impersonation and creates an admin client for calling
// the google admin api. If no serviceAccountFilePath is defined, the application default credential
// is used.
func createDirectoryService(serviceAccountFilePath, email string, logger log.Logger) (*admin.Service, error) {
	ctx := context.Background()
	// We know impersonation is required when using a service account credential
	// TODO: or is it?
	if email == "" && serviceAccountFilePath == "" {
		logger.Warn("creating directory service without service account file and admin email, assuming workload identity")
		return createDirectoryServiceWithWorkloadIdentity(ctx, logger)
	}

	var jsonCredentials []byte
	var err error

	if serviceAccountFilePath == "" {
		logger.Warn("the application default credential is used since the service account file path is not used")
		credential, err := google.FindDefaultCredentials(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch application default credentials: %w", err)
		}
		jsonCredentials = credential.JSON
	} else {
		jsonCredentials, err = os.ReadFile(serviceAccountFilePath)
		if err != nil {
			return nil, fmt.Errorf("error reading credentials from file: %v", err)
		}
	}
	config, err := google.JWTConfigFromJSON(jsonCredentials, admin.AdminDirectoryGroupReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("unable to parse client secret file to config: %v", err)
	}

	// Only attempt impersonation when there is a user configured
	if email != "" {
		config.Subject = email
	}

	return admin.NewService(ctx, option.WithHTTPClient(config.Client(ctx)))
}

func createDirectoryServiceWithWorkloadIdentity(ctx context.Context, logger log.Logger) (*admin.Service, error) {
	creds, err := google.FindDefaultCredentials(ctx, admin.AdminDirectoryGroupReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch application default credentials: %w", err)
	}

	return admin.NewService(ctx, option.WithTokenSource(creds.TokenSource))
}
