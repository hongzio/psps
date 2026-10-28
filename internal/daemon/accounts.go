package daemon

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Accounts are agent CLI credentials defined once and used by any number of
// isolations: a Claude token from `claude setup-token`, an API key. The
// secret is in the credentials file, read when an agent starts; agents get
// it as the kind's variable. Only kinds that stay valid wherever they are
// used at once qualify, which is why a ChatGPT login is not one.

const accountSection = "account"

func (m *Manager) accountView(a store.Account) api.Account {
	v := api.Account{Account: a}
	if k, ok := api.AccountKindNamed(a.Kind); ok {
		v.Tool = k.Tool
	}
	users, _ := m.store.AccountUsers(a.Name)
	v.UsedBy = m.isoNames(users)
	if _, err := m.accountSecret(a.Name); err != nil {
		v.SecretError = err.Error()
	}
	return v
}

func (m *Manager) accountSecret(name string) (string, error) {
	if m.creds == nil {
		return "", errors.New("no credentials file configured")
	}
	v, err := m.creds.Read(accountSection, name)
	if err != nil {
		var d *creds.Denied
		if errors.As(err, &d) {
			return "", fmt.Errorf("account %s has no secret stored; set it with `gero account set %s`", name, name)
		}
		return "", err
	}
	return v["token"], nil
}

func (m *Manager) Accounts() ([]api.Account, error) {
	as, err := m.store.Accounts()
	if err != nil {
		return nil, err
	}
	out := make([]api.Account, 0, len(as))
	for _, a := range as {
		out = append(out, m.accountView(a))
	}
	return out, nil
}

// checkSecret refuses a value that cannot be one of the kind's: pasting an
// API key where a token goes is the usual slip.
func checkSecret(k api.AccountKind, secret string) error {
	switch {
	case secret == "":
		return errors.New("the secret is empty")
	case strings.ContainsAny(secret, " \t\r\n"):
		return errors.New("the secret has spaces or line breaks in it")
	case k.Prefix != "" && !strings.HasPrefix(secret, k.Prefix):
		return fmt.Errorf("a %s starts with %s; this one does not (%s)", k.Name, k.Prefix, k.Summary)
	}
	return nil
}

// PutAccount adds an account, or replaces an account's secret. A new
// secret reaches the agents using it at their next idle moment.
func (m *Manager) PutAccount(name string, req api.PutAccount, create bool) (api.Account, error) {
	if m.creds == nil {
		return api.Account{}, errors.New("no credentials file configured")
	}
	if err := proxy.ValidEnvName(name); err != nil {
		return api.Account{}, badRequest(fmt.Errorf("account name: %w", err))
	}
	cur, err := m.store.Account(name)
	switch {
	case create && err == nil:
		return api.Account{}, badRequest(fmt.Errorf("account %s already exists; replace its secret with `gero account set %s`", name, name))
	case !create && err != nil:
		return api.Account{}, err
	case create:
		cur = store.Account{Name: name, Kind: req.Kind}
	}
	if !create && req.Kind != "" && req.Kind != cur.Kind {
		return api.Account{}, badRequest(fmt.Errorf("account %s is a %s; its kind cannot change", name, cur.Kind))
	}
	k, ok := api.AccountKindNamed(cur.Kind)
	if !ok {
		return api.Account{}, badRequest(fmt.Errorf("unknown account kind %q (%s)", cur.Kind, kindNames()))
	}
	secret := strings.TrimSpace(req.Secret)
	if err := checkSecret(k, secret); err != nil {
		return api.Account{}, badRequest(err)
	}
	if err := m.creds.Write(accountSection, name, map[string]string{"token": secret}); err != nil {
		return api.Account{}, err
	}
	if err := m.store.PutAccount(cur); err != nil {
		return api.Account{}, err
	}
	if create {
		m.log.Printf("account %s (%s) added", name, k.Name)
	} else {
		m.log.Printf("account %s: secret replaced", name)
		users, _ := m.store.AccountUsers(name)
		for _, iso := range users {
			m.cliChanged(iso, k.Tool)
		}
	}
	return m.accountView(cur), nil
}

func kindNames() string {
	var names []string
	for _, k := range api.AccountKinds {
		names = append(names, k.Name)
	}
	return strings.Join(names, ", ")
}

// DeleteAccount removes the account and its secret. One an isolation uses
// is kept unless forced, which leaves those isolations with a login inside
// them, if they have one.
func (m *Manager) DeleteAccount(name string, force bool) error {
	a, err := m.store.Account(name)
	if err != nil {
		return err
	}
	users, err := m.store.AccountUsers(name)
	if err != nil {
		return err
	}
	if len(users) > 0 && !force {
		return badRequest(fmt.Errorf("account %s is used by %s; stop using it there first, or force", name, strings.Join(m.isoNames(users), ", ")))
	}
	if err := m.store.DeleteAccount(name); err != nil {
		return err
	}
	if m.creds != nil {
		if err := m.creds.Write(accountSection, name, nil); err != nil {
			return err
		}
	}
	m.log.Printf("account %s removed", name)
	if k, ok := api.AccountKindNamed(a.Kind); ok {
		for _, iso := range users {
			m.cliChanged(iso, k.Tool)
		}
	}
	return nil
}

// UseAccount sets the account an isolation's CLI uses; "" goes back to a
// login inside the isolation. Its agents of that kind restart with it at
// their next idle moment.
func (m *Manager) UseAccount(name, toolName, account string) error {
	tool, err := cliTool(toolName)
	if err != nil {
		return err
	}
	iso, err := m.isolationNamed(name)
	if err != nil {
		return err
	}
	if account != "" {
		a, err := m.store.Account(account)
		if err != nil {
			return fmt.Errorf("account %s: %w", account, err)
		}
		k, ok := api.AccountKindNamed(a.Kind)
		if !ok || k.Tool != tool {
			return badRequest(fmt.Errorf("account %s is a %s, which %s cannot use", account, a.Kind, tool))
		}
	}
	if err := m.store.UseAccount(iso.ID, tool, account); err != nil {
		return err
	}
	if account == "" {
		m.log.Printf("isolation %s: %s uses a login inside it", iso.Name, tool)
	} else {
		m.log.Printf("isolation %s: %s uses account %s", iso.Name, tool, account)
	}
	m.cliChanged(iso.ID, tool)
	return nil
}

// accountEnv is what the isolation's (by ID) accounts give its agents: each one's
// variable. An account whose secret cannot be read is left out, and the
// CLI's status says why.
func (m *Manager) accountEnv(iso string) []string {
	used, err := m.store.AccountsUsed(iso)
	if err != nil {
		return nil
	}
	var env []string
	for _, name := range used {
		a, err := m.store.Account(name)
		if err != nil {
			continue
		}
		k, ok := api.AccountKindNamed(a.Kind)
		if !ok {
			continue
		}
		secret, err := m.accountSecret(name)
		if err != nil {
			m.log.Printf("isolation %s: %v", m.isoName(iso), err)
			continue
		}
		env = append(env, k.Env+"="+secret)
	}
	sort.Strings(env)
	return env
}

// migrateEnvAccounts moves a Claude token or API key set in an isolation's
// environment, where it used to go, into an account the isolation uses:
// out of the store and into the credentials file. It runs once.
func (m *Manager) migrateEnvAccounts() error {
	const key = "migrated:env-accounts"
	if _, done, err := m.store.Meta(key); err != nil || done {
		return err
	}
	if m.creds == nil {
		return nil
	}
	isos, err := m.store.Isolations()
	if err != nil {
		return err
	}
	moved := false
	for _, iso := range isos {
		used, err := m.store.AccountsUsed(iso.ID)
		if err != nil {
			return err
		}
		for _, k := range api.AccountKinds {
			secret := strings.TrimSpace(iso.Env[k.Env])
			if secret == "" || used[k.Tool] != "" || checkSecret(k, secret) != nil {
				continue
			}
			name, err := m.accountFor(k, secret, iso.Name)
			if err != nil {
				return err
			}
			if err := m.store.UseAccount(iso.ID, k.Tool, name); err != nil {
				return err
			}
			delete(iso.Env, k.Env)
			if err := m.store.PutIsolation(iso); err != nil {
				return err
			}
			used[k.Tool] = name
			moved = true
			m.log.Printf("isolation %s: %s moved from its environment to account %s", iso.Name, k.Env, name)
		}
	}
	if moved {
		// Not left behind in the database's free pages.
		if err := m.store.Vacuum(); err != nil {
			return err
		}
	}
	return m.store.SetMeta(key, "1")
}

// accountFor is an account of kind k holding secret: one already there, or
// a new one named after the CLI (or, if that is taken, the isolation).
func (m *Manager) accountFor(k api.AccountKind, secret, iso string) (string, error) {
	as, err := m.store.Accounts()
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, a := range as {
		taken[a.Name] = true
		if a.Kind == k.Name {
			if s, err := m.accountSecret(a.Name); err == nil && s == secret {
				return a.Name, nil
			}
		}
	}
	name := string(k.Tool)
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%s", iso, k.Tool)
		if i > 2 {
			name = fmt.Sprintf("%s-%s-%d", iso, k.Tool, i)
		}
	}
	if err := m.creds.Write(accountSection, name, map[string]string{"token": secret}); err != nil {
		return "", err
	}
	return name, m.store.PutAccount(store.Account{Name: name, Kind: k.Name})
}

// accountsOf is the account each of the isolation's (by ID) CLIs uses.
func (m *Manager) accountsOf(iso string) map[driver.Kind]string {
	used, _ := m.store.AccountsUsed(iso)
	if len(used) == 0 {
		return nil
	}
	return used
}
