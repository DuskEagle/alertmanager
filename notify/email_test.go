// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Some tests require a running mail catcher. We use MailDev for this purpose,
// it can work without or with authentication (LOGIN only). It exposes a REST
// API which we use to retrieve and check the sent emails.
//
// Those tests are only executed when specific environment variables are set,
// otherwise they are skipped. The tests must be run by the CI.
//
// To run the tests locally, you should start 2 MailDev containers:
//
// $ docker run --rm -p 1080:1080 -p 1025:1025 --entrypoint bin/maildev djfarrelly/maildev@sha256:624e0ec781e11c3531da83d9448f5861f258ee008c1b2da63b3248bfd680acfa -v
// $ docker run --rm -p 1081:1080 -p 1026:1025 --entrypoint bin/maildev djfarrelly/maildev@sha256:624e0ec781e11c3531da83d9448f5861f258ee008c1b2da63b3248bfd680acfa --incoming-user user --incoming-pass pass -v
//
// $ EMAIL_NO_AUTH_CONFIG=testdata/email_noauth.yml EMAIL_AUTH_CONFIG=testdata/email_auth.yml make
//
// See also https://github.com/djfarrelly/MailDev for more details.
package notify

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	yaml "gopkg.in/yaml.v2"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

const (
	emailNoAuthConfigVar = "EMAIL_NO_AUTH_CONFIG"
	emailAuthConfigVar   = "EMAIL_AUTH_CONFIG"

	emailTo   = "alerts@example.com"
	emailFrom = "alertmanager@example.com"
)

// email represents an email returned by the MailDev REST API.
// See https://github.com/djfarrelly/MailDev/blob/master/docs/rest.md.
type email struct {
	To      []map[string]string
	From    []map[string]string
	Subject string
	HTML    *string
	Text    *string
	Headers map[string]string
}

// mailDev is a client for the MailDev server.
type mailDev struct {
	*url.URL
}

func (m *mailDev) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	urlp, err := url.Parse(s)
	if err != nil {
		return err
	}
	m.URL = urlp
	return nil
}

// getLastEmail returns the last received email.
func (m *mailDev) getLastEmail() (*email, error) {
	code, b, err := m.doEmailRequest(http.MethodGet, "/email")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("expected status OK, got %d", code)
	}

	var emails []email
	err = yaml.Unmarshal(b, &emails)
	if err != nil {
		return nil, err
	}
	if len(emails) == 0 {
		return nil, fmt.Errorf("expected non-empty list of emails")
	}
	return &emails[len(emails)-1], nil
}

// deleteAllEmails deletes all emails.
func (m *mailDev) deleteAllEmails() error {
	_, _, err := m.doEmailRequest(http.MethodDelete, "/email/all")
	return err
}

// doEmailRequest makes a request to the MailDev API.
func (m *mailDev) doEmailRequest(method string, path string) (int, []byte, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("%s://%s%s", m.Scheme, m.Host, path), nil)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	req = req.WithContext(ctx)
	defer cancel()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return 0, nil, err
	}
	return res.StatusCode, b, nil
}

// emailTestConfig is the configuration for the tests.
type emailTestConfig struct {
	Smarthost string   `yaml:"smarthost"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
	Server    *mailDev `yaml:"server"`
}

func loadEmailTestConfiguration(f string) (emailTestConfig, error) {
	c := emailTestConfig{}
	b, err := ioutil.ReadFile(f)
	if err != nil {
		return c, err
	}

	err = yaml.UnmarshalStrict(b, &c)
	if err != nil {
		return c, err
	}

	return c, nil
}

// notifyEmail sends a notification with one firing alert and retrieves the
// email from the SMTP server.
func notifyEmail(cfg *config.EmailConfig, server *mailDev) (*email, bool, error) {
	if cfg.RequireTLS == nil {
		cfg.RequireTLS = new(bool)
	}
	if cfg.Headers == nil {
		cfg.Headers = make(map[string]string)
	}
	firingAlert := &types.Alert{
		Alert: model.Alert{
			Labels:   model.LabelSet{},
			StartsAt: time.Now(),
			EndsAt:   time.Now().Add(time.Hour),
		},
	}
	err := server.deleteAllEmails()
	if err != nil {
		return nil, false, err
	}

	tmpl, err := template.FromGlobs()
	if err != nil {
		return nil, false, err
	}
	tmpl.ExternalURL, _ = url.Parse("http://am")
	email := NewEmail(cfg, tmpl, log.NewNopLogger())

	ctx := context.Background()
	retry, err := email.Notify(ctx, firingAlert)
	if err != nil {
		return nil, retry, err
	}

	e, err := server.getLastEmail()
	return e, retry, err
}

// TestEmailNotifyWithoutAuthentication sends an email to an instance of
// MailDev configured with no authentication then it checks that the server has
// successfully processed the email.
func TestEmailNotifyWithoutAuthentication(t *testing.T) {
	cfgFile := os.Getenv(emailNoAuthConfigVar)
	if len(cfgFile) == 0 {
		t.Skipf("%s not set", emailNoAuthConfigVar)
	}

	c, err := loadEmailTestConfiguration(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = notifyEmail(
		&config.EmailConfig{
			Smarthost: c.Smarthost,
			To:        emailTo,
			From:      emailFrom,
			HTML:      "HTML body",
			Text:      "Text body",
		},
		c.Server,
	)
	require.NoError(t, err)
}

// TestEmailNotifyWithSTARTTLS connects to the server, upgrades the connection
// to TLS, sends an email then it checks that the server has successfully
// processed the email.
// MailDev doesn't support STARTTLS and authentication at the same time so it
// is the only way to test successful STARTTLS.
func TestEmailNotifyWithSTARTTLS(t *testing.T) {
	cfgFile := os.Getenv(emailNoAuthConfigVar)
	if len(cfgFile) == 0 {
		t.Skipf("%s not set", emailNoAuthConfigVar)
	}

	c, err := loadEmailTestConfiguration(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	trueVar := true
	_, _, err = notifyEmail(
		&config.EmailConfig{
			Smarthost:  c.Smarthost,
			To:         emailTo,
			From:       emailFrom,
			HTML:       "HTML body",
			Text:       "Text body",
			RequireTLS: &trueVar,
			// MailDev embeds a self-signed certificate which can't be retrieved.
			TLSConfig: commoncfg.TLSConfig{InsecureSkipVerify: true},
		},
		c.Server,
	)
	require.NoError(t, err)
}

// TestEmailNotifyWithAuthentication sends emails to an instance of MailDev
// configured with authentication.
func TestEmailNotifyWithAuthentication(t *testing.T) {
	cfgFile := os.Getenv(emailAuthConfigVar)
	if len(cfgFile) == 0 {
		t.Skipf("%s not set", emailAuthConfigVar)
	}

	c, err := loadEmailTestConfiguration(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		updateCfg func(*config.EmailConfig)

		errMsg string
		retry  bool
	}{
		{
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
			},
		},
		{
			// HTML-only email.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
				cfg.Text = ""
			},
		},
		{
			// text-only email.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
				cfg.HTML = ""
			},
		},
		{
			// Multiple To addresses.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
				cfg.To = strings.Join([]string{emailTo, emailFrom}, ",")
			},
		},
		{
			// No more than one From address.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
				cfg.From = strings.Join([]string{emailFrom, emailTo}, ",")
			},

			errMsg: "must be exactly one from address",
			retry:  false,
		},
		{
			// Wrong credentials.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password + "wrong")
			},

			errMsg: "Invalid username or password",
			retry:  true,
		},
		{
			// No credentials.
			errMsg: "authentication Required",
			retry:  true,
		},
		{
			// Fail to enable STARTTLS.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.RequireTLS = new(bool)
				*cfg.RequireTLS = true
			},

			errMsg: "does not advertise the STARTTLS extension",
			retry:  true,
		},
		{
			// Invalid Hello string.
			updateCfg: func(cfg *config.EmailConfig) {
				cfg.AuthUsername = c.Username
				cfg.AuthPassword = config.NewSecret(c.Password)
				cfg.Hello = "invalid hello string"
			},

			errMsg: "501 Error",
			retry:  true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run("", func(t *testing.T) {
			emailCfg := &config.EmailConfig{
				Smarthost: c.Smarthost,
				To:        emailTo,
				From:      emailFrom,
				HTML:      "HTML body",
				Text:      "Text body",
				Headers: map[string]string{
					"Subject": "{{ len .Alerts }} {{ .Status }} alert(s)",
				},
			}
			if tc.updateCfg != nil {
				tc.updateCfg(emailCfg)
			}

			e, retry, err := notifyEmail(emailCfg, c.Server)
			if len(tc.errMsg) > 0 {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errMsg)
				require.Equal(t, tc.retry, retry)
				return
			}
			require.NoError(t, err)

			require.Equal(t, "1 firing alert(s)", e.Subject)

			getAddresses := func(addresses []map[string]string) []string {
				res := make([]string, 0, len(addresses))
				for _, addr := range addresses {
					res = append(res, addr["address"])
				}
				return res
			}
			to := getAddresses(e.To)
			from := getAddresses(e.From)
			require.Equal(t, strings.Split(emailCfg.To, ","), to)
			require.Equal(t, strings.Split(emailCfg.From, ","), from)

			if len(emailCfg.HTML) > 0 {
				require.Equal(t, emailCfg.HTML, *e.HTML)
			} else {
				require.Nil(t, e.HTML)
			}

			if len(emailCfg.Text) > 0 {
				require.Equal(t, emailCfg.Text, *e.Text)
			} else {
				require.Nil(t, e.Text)
			}
		})
	}
}

func TestEmailConfigNoAuthMechs(t *testing.T) {
	email := &Email{
		conf: &config.EmailConfig{AuthUsername: "test"}, tmpl: &template.Template{}, logger: log.NewNopLogger(),
	}
	_, err := email.auth("")
	require.Error(t, err)
	require.Equal(t, err.Error(), "unknown auth mechanism: ")
}

func TestEmailConfigMissingAuthParam(t *testing.T) {
	conf := &config.EmailConfig{AuthUsername: "test"}
	email := &Email{
		conf: conf, tmpl: &template.Template{}, logger: log.NewNopLogger(),
	}
	_, err := email.auth("CRAM-MD5")
	require.Error(t, err)
	require.Equal(t, err.Error(), "missing secret for CRAM-MD5 auth mechanism")

	_, err = email.auth("PLAIN")
	require.Error(t, err)
	require.Equal(t, err.Error(), "missing password for PLAIN auth mechanism")

	_, err = email.auth("LOGIN")
	require.Error(t, err)
	require.Equal(t, err.Error(), "missing password for LOGIN auth mechanism")

	_, err = email.auth("PLAIN LOGIN")
	require.Error(t, err)
	require.Equal(t, err.Error(), "missing password for PLAIN auth mechanism; missing password for LOGIN auth mechanism")
}

func TestEmailNoUsernameStillOk(t *testing.T) {
	email := &Email{
		conf: &config.EmailConfig{}, tmpl: &template.Template{}, logger: log.NewNopLogger(),
	}
	a, err := email.auth("CRAM-MD5")
	require.NoError(t, err)
	require.Nil(t, a)
}
