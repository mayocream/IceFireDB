// Package credentials provides credential retrieval and management
//
// The Credentials is the primary method of getting access to and managing
// credentials Values. Using dependency injection retrieval of the credential
// values is handled by a object which satisfies the Provider interface.
//
// By default the Credentials.Get() will cache the successful result of a
// Provider's Retrieve() until Provider.IsExpired() returns true. At which
// point Credentials will call Provider's Retrieve() to get new credential Value.
//
// The Provider is responsible for determining when credentials Value have expired.
// It is also important to note that Credentials will always call Retrieve the
// first time Credentials.Get() is called.
//
// Example of using the environment variable credentials.
//
//	creds := credentials.NewEnvCredentials()
//
//	// Retrieve the credentials value
//	credValue, err := creds.Get()
//	if err != nil {
//	    // handle error
//	}
//
// Example of forcing credentials to expire and be refreshed on the next Get().
// This may be helpful to proactively expire credentials and refresh them sooner
// than they would naturally expire on their own.
//
//	creds := credentials.NewCredentials(&ec2rolecreds.EC2RoleProvider{})
//	creds.Expire()
//	credsValue, err := creds.Get()
//	// New credentials will be retrieved instead of from cache.
//
// # Custom Provider
//
// Each Provider built into this package also provides a helper method to generate
// a Credentials pointer setup with the provider. To use a custom Provider just
// create a type which satisfies the Provider interface and pass it to the
// NewCredentials method.
//
//	type MyProvider struct{}
//	func (m *MyProvider) Retrieve() (Value, error) {...}
//	func (m *MyProvider) IsExpired() bool {...}
//
//	creds := credentials.NewCredentials(&MyProvider{})
//	credValue, err := creds.Get()
//
// Deprecated: aws-sdk-go is deprecated. Use aws-sdk-go-v2.
// See https://aws.amazon.com/blogs/developer/announcing-end-of-support-for-aws-sdk-for-go-v1-on-july-31-2025/.
package credentials

import (
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/internal/sync/singleflight"
)

// AnonymousCredentials is an empty Credential object that can be used as
// dummy placeholder credentials for requests that do not need signed.
//
// This Credentials can be used to configure a service to not sign requests
// when making service API calls. For example, when accessing public
// s3 buckets.
//
//	svc := s3.New(session.Must(session.NewSession(&aws.Config{
//	  Credentials: credentials.AnonymousCredentials,
//	})))
//	// Access public S3 buckets.
var AnonymousCredentials = NewStaticCredentials("", "", "")

// A Value is the AWS credentials value for individual credential fields.
type Value struct {
	// AWS Access key ID
	AccessKeyID string

	// AWS Secret Access Key
	SecretAccessKey string

	// AWS Session Token
	SessionToken string

	// Provider used to get credentials
	ProviderName string
}

// HasKeys returns if the credentials Value has both AccessKeyID and
// SecretAccessKey value set.
func (v Value) HasKeys() bool {
	return len(v.AccessKeyID) != 0 && len(v.SecretAccessKey) != 0
}

// A Provider is the interface for any component which will provide credentials
// Value. A provider is required to manage its own Expired state, and what to
// be expired means.
//
// The Provider should not need to implement its own mutexes, because
// that will be managed by Credentials.
type Provider interface {
	// Retrieve returns nil if it successfully retrieved the value.
	// Error is returned if the value were not obtainable, or empty.
	Retrieve() (Value, error)

	// IsExpired returns if the credentials are no longer valid, and need
	// to be retrieved.
	IsExpired() bool
}

// ProviderWithContext is a Provider that can retrieve credentials with a Context
type ProviderWithContext interface {
	Provider

	RetrieveWithContext(Context) (Value, error)
}

// An Expirer is an interface that Providers can implement to expose the expiration
// time, if known.  If the Provider cannot accurately provide this info,
// it should not implement this interface.
type Expirer interface {
	// The time at which the credentials are no longer valid
	ExpiresAt() time.Time
}

// An ErrorProvider is a stub credentials provider that always returns an error
// this is used by the SDK when construction a known provider is not possible
// due to an error.
type ErrorProvider struct {
	// The error to be returned from Retrieve
	Err error

	// The provider name to set on the Retrieved returned Value
	ProviderName string
}

// Retrieve will always return the error that the ErrorProvider was created with.
func (p ErrorProvider) Retrieve() (Value, error) {
	return Value{ProviderName: p.ProviderName}, p.Err
}

// IsExpired will always return not expired.
func (p ErrorProvider) IsExpired() bool {
	return false
}

// A Expiry provides shared expiration logic to be used by credentials
// providers to implement expiry functionality.
//
// The best method to use this struct is as an anonymous field within the
// provider's struct.
//
// Example:
//
//	type EC2RoleProvider struct {
//	    Expiry
//	    ...
//	}
type Expiry struct {
	// The date/time when to expire on
	expiration time.Time

	// If set will be used by IsExpired to determine the current time.
	// Defaults to time.Now if CurrentTime is not set.  Available for testing
	// to be able to mock out the current time.
	CurrentTime func() time.Time
}

// SetExpiration sets the expiration IsExpired will check when called.
//
// If window is greater than 0 the expiration time will be reduced by the
// window value.
//
// Using a window is helpful to trigger credentials to expire sooner than
// the expiration time given to ensure no requests are made with expired
// tokens.
func (e *Expiry) SetExpiration(expiration time.Time, window time.Duration) {
	// Passed in expirations should have the monotonic clock values stripped.
	// This ensures time comparisons will be based on wall-time.
	e.expiration = expiration.Round(0)
	if window > 0 {
		e.expiration = e.expiration.Add(-window)
	}
}

// IsExpired returns if the credentials are expired.
func (e *Expiry) IsExpired() bool {
	curTime := e.CurrentTime
	if curTime == nil {
		curTime = time.Now
	}
	return e.expiration.Before(curTime())
}

// ExpiresAt returns the expiration time of the credential
func (e *Expiry) ExpiresAt() time.Time {
	return e.expiration
}

// A Credentials provides concurrency safe retrieval of AWS credentials Value.
// Credentials will cache the credentials value until they expire. Once the value
// expires the next Get will attempt to retrieve valid credentials.
//
// Credentials is safe to use across multiple goroutines and will manage the
// synchronous state so the Providers do not need to implement their own
// synchronization.
//
// The first Credentials.Get() will always call Provider.Retrieve() to get the
// first instance of the credentials Value. All calls to Get() after that
// will return the cached credentials Value until IsExpired() returns true.
type Credentials struct {
	sf singleflight.Group

	m        sync.RWMutex
	creds    Value
	provider Provider
}

// NewCredentials returns a pointer to a new Credentials with the provider set.
func NewCredentials(provider Provider) *Credentials {
	c := &Credentials{
		provider: provider,
	}
	return c
}

// GetWithContext returns the credentials value, or error if the credentials
// Value failed to be retrieved. Will return early if the passed in context is
// canceled.
//
// Will return the cached credentials Value if it has not expired. If the
// credentials Value has expired the Provider's Retrieve() will be called
// to refresh the credentials.
//
// If Credentials.Expire() was called the credentials Value will be force
// expired, and the next call to Get() will cause them to be refreshed.
//
// Passed in Context is equivalent to aws.Context, and context.Context.
func (c *Credentials) GetWithContext(ctx Context) (Value, error) {
	// Check if credentials are cached, and not expired.
	select {
	case curCreds, ok := <-c.asyncIsExpired():
		// ok will only be true, of the credentials were not expired. ok will
		// be false and have no value if the credentials are expired.
		if ok {
			return curCreds, nil
		}
	case <-ctx.Done():
		return Value{}, awserr.New("RequestCanceled",
			"request context canceled", ctx.Err())
	}

	// Cannot pass context down to the actual retrieve, because the first
	// context would cancel the whole group when there is not direct
	// association of items in the group.
	resCh := c.sf.DoChan("", func() (interface{}, error) {
		return c.singleRetrieve(&suppressedContext{ctx})
	})
	select {
	case res := <-resCh:
		return res.Val.(Value), res.Err
	case <-ctx.Done():
		return Value{}, awserr.New("RequestCanceled",
			"request context canceled", ctx.Err())
	}
}

func (c *Credentials) singleRetrieve(ctx Context) (interface{}, error) {
	c.m.Lock()
	defer c.m.Unlock()

	if curCreds := c.creds; !c.isExpiredLocked(curCreds) {
		return curCreds, nil
	}

	var creds Value
	var err error
	if p, ok := c.provider.(ProviderWithContext); ok {
		creds, err = p.RetrieveWithContext(ctx)
	} else {
		creds, err = c.provider.Retrieve()
	}
	if err == nil {
		c.creds = creds
	}

	return creds, err
}

// Get returns the credentials value, or error if the credentials Value failed
// to be retrieved.
//
// Will return the cached credentials Value if it has not expired. If the
// credentials Value has expired the Provider's Retrieve() will be called
// to refresh the credentials.
//
// If Credentials.Expire() was called the credentials Value will be force
// expired, and the next call to Get() will cause them to be refreshed.
func (c *Credentials) Get() (Value, error) {
	return c.GetWithContext(backgroundContext())
}

// Expire expires the credentials and forces them to be retrieved on the
// next call to Get().
//
// This will override the Provider's expired state, and force Credentials
// to call the Provider's Retrieve().
func (c *Credentials) Expire() {
	c.m.Lock()
	defer c.m.Unlock()

	c.creds = Value{}
}

// IsExpired returns if the credentials are no longer valid, and need
// to be retrieved.
//
// If the Credentials were forced to be expired with Expire() this will
// reflect that override.
func (c *Credentials) IsExpired() bool {
	c.m.RLock()
	defer c.m.RUnlock()

	return c.isExpiredLocked(c.creds)
}

// asyncIsExpired returns a channel of credentials Value. If the channel is
// closed the credentials are expired and credentials value are not empty.
func (c *Credentials) asyncIsExpired() <-chan Value {
	ch := make(chan Value, 1)
	go func() {
		c.m.RLock()
		defer c.m.RUnlock()

		if curCreds := c.creds; !c.isExpiredLocked(curCreds) {
			ch <- curCreds
		}

		close(ch)
	}()

	return ch
}

// isExpiredLocked helper method wrapping the definition of expired credentials.
func (c *Credentials) isExpiredLocked(creds interface{}) bool {
	return creds == nil || creds.(Value) == Value{} || c.provider.IsExpired()
}

// ExpiresAt provides access to the functionality of the Expirer interface of
// the underlying Provider, if it supports that interface.  Otherwise, it returns
// an error.
func (c *Credentials) ExpiresAt() (time.Time, error) {
	c.m.RLock()
	defer c.m.RUnlock()

	expirer, ok := c.provider.(Expirer)
	if !ok {
		return time.Time{}, awserr.New("ProviderNotExpirer",
			fmt.Sprintf("provider %s does not support ExpiresAt()",
				c.creds.ProviderName),
			nil)
	}
	if c.creds == (Value{}) {
		// set expiration time to the distant past
		return time.Time{}, nil
	}
	return expirer.ExpiresAt(), nil
}

type suppressedContext struct {
	Context
}

func (s *suppressedContext) Deadline() (deadline time.Time, ok bool) {
	return time.Time{}, false
}

func (s *suppressedContext) Done() <-chan struct{} {
	return nil
}

func (s *suppressedContext) Err() error {
	return nil
}
