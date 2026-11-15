// Package keyring is a minimal client for the freedesktop Secret Service API
// (org.freedesktop.secrets), as provided by GNOME Keyring and KWallet.
package keyring

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

var (
	ErrUnavailable = errors.New("secret service unavailable")
	ErrNotFound    = errors.New("secret not found")
)

const (
	busName        = "org.freedesktop.secrets"
	servicePath    = dbus.ObjectPath("/org/freedesktop/secrets")
	ifaceService   = "org.freedesktop.Secret.Service"
	ifaceCollect   = "org.freedesktop.Secret.Collection"
	ifaceItem      = "org.freedesktop.Secret.Item"
	ifacePrompt    = "org.freedesktop.Secret.Prompt"
	ifaceSession   = "org.freedesktop.Secret.Session"
	callTimeout    = 5 * time.Second
	promptTimeout  = 60 * time.Second
	connectTimeout = 2 * time.Second
	noPrompt       = dbus.ObjectPath("/")
)

// secret mirrors the Secret Service (oayays) struct.
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

func attributes(service, user string) map[string]string {
	return map[string]string{
		"application": "navigo",
		"service":     service,
		"username":    user,
	}
}

func label(user string) string {
	return "Navigo password for " + user
}

type client struct {
	conn    *dbus.Conn
	cancel  context.CancelFunc
	session dbus.ObjectPath
}

func connect() (*client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancelling the connection context closes it, unblocking a hung handshake.
	timer := time.AfterFunc(connectTimeout, cancel)
	conn, err := dbus.SessionBusPrivateNoAutoStartup(dbus.WithContext(ctx))
	if err != nil {
		timer.Stop()
		cancel()
		return nil, ErrUnavailable
	}
	if err := conn.Auth(nil); err != nil {
		timer.Stop()
		cancel()
		conn.Close()
		return nil, ErrUnavailable
	}
	if err := conn.Hello(); err != nil {
		timer.Stop()
		cancel()
		conn.Close()
		return nil, ErrUnavailable
	}
	timer.Stop()
	return &client{conn: conn, cancel: cancel}, nil
}

func (c *client) close() {
	if c.session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		c.conn.Object(busName, c.session).CallWithContext(ctx, ifaceSession+".Close", 0)
		cancel()
	}
	c.conn.Close()
	c.cancel()
}

func (c *client) call(timeout time.Duration, path dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.conn.Object(busName, path).CallWithContext(ctx, method, 0, args...)
}

func (c *client) available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var has bool
	err := c.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, busName).Store(&has)
	if err == nil && has {
		return true
	}
	// The service may be D-Bus activatable without currently running.
	var names []string
	if err := c.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.ListActivatableNames", 0).Store(&names); err != nil {
		return false
	}
	for _, n := range names {
		if n == busName {
			return true
		}
	}
	return false
}

func wrap(err error) error {
	var dErr dbus.Error
	if errors.As(err, &dErr) {
		switch dErr.Name {
		case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return err
}

func (c *client) openSession() error {
	var out dbus.Variant
	var path dbus.ObjectPath
	err := c.call(callTimeout, servicePath, ifaceService+".OpenSession", "plain", dbus.MakeVariant("")).Store(&out, &path)
	if err != nil {
		return wrap(err)
	}
	c.session = path
	return nil
}

// prompt runs a Secret Service prompt and waits for its Completed signal.
// It returns the result variant, or an error if dismissed or timed out.
func (c *client) prompt(p dbus.ObjectPath) (dbus.Variant, error) {
	if p == "" || p == noPrompt {
		return dbus.Variant{}, nil
	}
	ch := make(chan *dbus.Signal, 8)
	c.conn.Signal(ch)
	defer c.conn.RemoveSignal(ch)
	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(p),
		dbus.WithMatchInterface(ifacePrompt),
		dbus.WithMatchMember("Completed"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), promptTimeout)
	defer cancel()
	if err := c.conn.AddMatchSignalContext(ctx, match...); err != nil {
		return dbus.Variant{}, wrap(err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), callTimeout)
		c.conn.RemoveMatchSignalContext(rctx, match...)
		rcancel()
	}()
	if err := c.call(callTimeout, p, ifacePrompt+".Prompt", "").Err; err != nil {
		return dbus.Variant{}, wrap(err)
	}
	for {
		select {
		case <-ctx.Done():
			c.call(callTimeout, p, ifacePrompt+".Dismiss")
			return dbus.Variant{}, errors.New("keyring: prompt timed out")
		case sig, ok := <-ch:
			if !ok {
				return dbus.Variant{}, ErrUnavailable
			}
			if sig.Path != p || sig.Name != ifacePrompt+".Completed" || len(sig.Body) < 2 {
				continue
			}
			if dismissed, _ := sig.Body[0].(bool); dismissed {
				return dbus.Variant{}, errors.New("keyring: prompt dismissed")
			}
			v, _ := sig.Body[1].(dbus.Variant)
			return v, nil
		}
	}
}

func (c *client) unlock(objs []dbus.ObjectPath) error {
	if len(objs) == 0 {
		return nil
	}
	var unlocked []dbus.ObjectPath
	var p dbus.ObjectPath
	if err := c.call(callTimeout, servicePath, ifaceService+".Unlock", objs).Store(&unlocked, &p); err != nil {
		return wrap(err)
	}
	_, err := c.prompt(p)
	return err
}

func (c *client) search(service, user string) (unlocked, locked []dbus.ObjectPath, err error) {
	err = c.call(callTimeout, servicePath, ifaceService+".SearchItems", attributes(service, user)).Store(&unlocked, &locked)
	return unlocked, locked, wrap(err)
}

func (c *client) defaultCollection() (dbus.ObjectPath, error) {
	var col dbus.ObjectPath
	if err := c.call(callTimeout, servicePath, ifaceService+".ReadAlias", "default").Store(&col); err != nil {
		return "", wrap(err)
	}
	if col != noPrompt && col != "" {
		return col, nil
	}
	// No default collection yet: ask the service to create one.
	props := map[string]dbus.Variant{
		"org.freedesktop.Secret.Collection.Label": dbus.MakeVariant("Login"),
	}
	var p dbus.ObjectPath
	if err := c.call(callTimeout, servicePath, ifaceService+".CreateCollection", props, "default").Store(&col, &p); err != nil {
		return "", wrap(err)
	}
	if col == noPrompt || col == "" {
		v, err := c.prompt(p)
		if err != nil {
			return "", err
		}
		col, _ = v.Value().(dbus.ObjectPath)
		if col == "" || col == noPrompt {
			return "", errors.New("keyring: could not create default collection")
		}
	}
	return col, nil
}

func (c *client) isLocked(path dbus.ObjectPath, iface string) (bool, error) {
	var v dbus.Variant
	if err := c.call(callTimeout, path, "org.freedesktop.DBus.Properties.Get", iface, "Locked").Store(&v); err != nil {
		return false, wrap(err)
	}
	b, _ := v.Value().(bool)
	return b, nil
}

func open() (*client, error) {
	c, err := connect()
	if err != nil {
		return nil, err
	}
	if err := c.openSession(); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// Available reports whether a Secret Service provider is reachable on the session bus.
func Available() bool {
	c, err := connect()
	if err != nil {
		return false
	}
	defer c.close()
	return c.available()
}

// Get returns the stored secret for the given service and user.
func Get(service, user string) (string, error) {
	c, err := open()
	if err != nil {
		return "", err
	}
	defer c.close()
	unlocked, locked, err := c.search(service, user)
	if err != nil {
		return "", err
	}
	if len(unlocked) == 0 && len(locked) > 0 {
		if err := c.unlock(locked[:1]); err != nil {
			return "", err
		}
		unlocked = locked[:1]
	}
	if len(unlocked) == 0 {
		return "", ErrNotFound
	}
	var s secret
	if err := c.call(callTimeout, unlocked[0], ifaceItem+".GetSecret", c.session).Store(&s); err != nil {
		return "", wrap(err)
	}
	return string(s.Value), nil
}

// Set stores (or replaces) the secret for the given service and user in the
// default collection.
func Set(service, user, password string) error {
	c, err := open()
	if err != nil {
		return err
	}
	defer c.close()
	col, err := c.defaultCollection()
	if err != nil {
		return err
	}
	if locked, err := c.isLocked(col, ifaceCollect); err != nil {
		return err
	} else if locked {
		if err := c.unlock([]dbus.ObjectPath{col}); err != nil {
			return err
		}
	}
	props := map[string]dbus.Variant{
		ifaceItem + ".Label":      dbus.MakeVariant(label(user)),
		ifaceItem + ".Attributes": dbus.MakeVariant(attributes(service, user)),
	}
	sec := secret{
		Session:     c.session,
		Parameters:  []byte{},
		Value:       []byte(password),
		ContentType: "text/plain; charset=utf8",
	}
	var item, p dbus.ObjectPath
	if err := c.call(callTimeout, col, ifaceCollect+".CreateItem", props, sec, true).Store(&item, &p); err != nil {
		return wrap(err)
	}
	if item == noPrompt || item == "" {
		if _, err := c.prompt(p); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes all secrets stored for the given service and user.
// It returns ErrNotFound if none exist.
func Delete(service, user string) error {
	c, err := open()
	if err != nil {
		return err
	}
	defer c.close()
	unlocked, locked, err := c.search(service, user)
	if err != nil {
		return err
	}
	if len(locked) > 0 {
		if err := c.unlock(locked); err != nil {
			return err
		}
	}
	items := append(unlocked, locked...)
	if len(items) == 0 {
		return ErrNotFound
	}
	for _, it := range items {
		var p dbus.ObjectPath
		if err := c.call(callTimeout, it, ifaceItem+".Delete").Store(&p); err != nil {
			return wrap(err)
		}
		if _, err := c.prompt(p); err != nil {
			return err
		}
	}
	return nil
}
