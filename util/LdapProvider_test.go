package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveLdapProviders_Order(t *testing.T) {
	conf := &ConfigType{
		LdapEnable: true,
		LdapServer: "legacy.example.com:389",
		LdapProviders: map[string]LdapProvider{
			"corp":   {DisplayName: "Corp AD", Server: "corp.example.com:389", Order: 2},
			"berlin": {DisplayName: "Berlin", Server: "berlin.example.com:389", Order: 1},
			// Reserved ID must be skipped: it would collide with the legacy provider.
			"ldap": {DisplayName: "Impostor", Server: "evil.example.com:389"},
		},
	}

	providers := conf.ActiveLdapProviders()

	require.Len(t, providers, 3)
	// Legacy flat config always first, right after the internal login tab.
	assert.Equal(t, "ldap", providers[0].ID)
	assert.Equal(t, "legacy.example.com:389", providers[0].Provider.Server)
	assert.Equal(t, "berlin", providers[1].ID)
	assert.Equal(t, "corp", providers[2].ID)
}

func TestActiveLdapProviders_NewConfigOnly(t *testing.T) {
	conf := &ConfigType{
		LdapProviders: map[string]LdapProvider{
			"corp": {DisplayName: "Corp AD", Server: "corp.example.com:389"},
		},
	}

	providers := conf.ActiveLdapProviders()

	require.Len(t, providers, 1)
	assert.Equal(t, "corp", providers[0].ID)
}

func TestActiveLdapProviders_Empty(t *testing.T) {
	conf := &ConfigType{}
	assert.Empty(t, conf.ActiveLdapProviders())
}

func TestGetLdapProvider(t *testing.T) {
	conf := &ConfigType{
		LdapEnable: true,
		LdapServer: "legacy.example.com:389",
		LdapProviders: map[string]LdapProvider{
			"corp": {Server: "corp.example.com:389"},
		},
	}

	p, ok := conf.GetLdapProvider("corp")
	require.True(t, ok)
	assert.Equal(t, "corp.example.com:389", p.Server)

	p, ok = conf.GetLdapProvider("ldap")
	require.True(t, ok)
	assert.Equal(t, "legacy.example.com:389", p.Server)

	_, ok = conf.GetLdapProvider("nope")
	assert.False(t, ok)
}

func TestLdapProvider_GetMappings_DefaultsWhenNil(t *testing.T) {
	p := LdapProvider{}
	m := p.GetMappings()
	require.NotNil(t, m)
	assert.Equal(t, "dn", m.DN)
	assert.Equal(t, "mail", m.Mail)
	assert.Equal(t, "uid", m.UID)
	assert.Equal(t, "cn", m.CN)
}

// Order is the field an administrator sets to control the login tabs; the ID is
// only the tie-break. A sort that consulted the ID first would look correct
// whenever the two agree, so this fixture makes them disagree.
func TestActiveLdapProviders_OrderBeatsID(t *testing.T) {
	conf := &ConfigType{
		LdapProviders: map[string]LdapProvider{
			"alpha": {Server: "alpha.example.com:389", Order: 2},
			"beta":  {Server: "beta.example.com:389", Order: 1},
		},
	}

	providers := conf.ActiveLdapProviders()

	require.Len(t, providers, 2)
	assert.Equal(t, "beta", providers[0].ID)
	assert.Equal(t, "alpha", providers[1].ID)
}

// Providers are collected from a map, so their input order is random on every
// call. Equal Order values therefore have to be resolved by a total order, or
// the login page reshuffles between requests; one pass could pass by luck.
func TestActiveLdapProviders_EqualOrderIsStable(t *testing.T) {
	conf := &ConfigType{
		LdapProviders: map[string]LdapProvider{
			"gamma": {Server: "gamma.example.com:389"},
			"alpha": {Server: "alpha.example.com:389"},
			"beta":  {Server: "beta.example.com:389"},
		},
	}

	for i := 0; i < 32; i++ {
		providers := conf.ActiveLdapProviders()

		require.Len(t, providers, 3)
		assert.Equal(t, []string{"alpha", "beta", "gamma"},
			[]string{providers[0].ID, providers[1].ID, providers[2].ID})
	}
}
