package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// System verifies Windows accounts: the password through LogonUser, and
// that the account may use the container runtime, i.e. open its named pipe
// (for Docker: members of docker-users or Administrators). Without a pipe
// (wslc) only Administrators and the account ContainerUI runs as are allowed.
type System struct {
	pipe string
	log  *slog.Logger
	// logon is LogonUser, a field so that a test can stand in for it.
	logon func(user, domain, password string) (windows.Token, error)
}

// NewSystem checks that LogonUser is available. service is unused on
// Windows (it names the PAM service on Linux); socket is the runtime's named
// pipe or "".
func NewSystem(service, socket string, log *slog.Logger) (*System, error) {
	if err := procLogonUserW.Find(); err != nil {
		return nil, fmt.Errorf("windows sign-in: %w", err)
	}
	return &System{pipe: socket, log: log, logon: logonUser}, nil
}

func (s *System) Verify(username, password string) bool {
	if username == "" || password == "" || len(username) > 256 || strings.ContainsRune(username+password, 0) {
		return false
	}
	user, domain := splitAccount(username)
	if user == "" {
		return false
	}
	token, err := s.logon(user, domain, password)
	if err != nil {
		s.log.Debug("windows login failed", "user", username, "error", err)
		return false
	}
	defer token.Close()
	if err := s.mayUseRuntime(token); err != nil {
		s.log.Warn("windows login refused: no access to the container runtime", "user", username, "error", err)
		return false
	}
	return true
}

// splitAccount splits DOMAIN\user into user and domain. A UPN (user@domain)
// and a bare name are passed whole with no domain, which LogonUser resolves:
// a bare name against the local accounts first, then the trusted domains.
func splitAccount(account string) (user, domain string) {
	if d, u, ok := strings.Cut(account, `\`); ok {
		return u, d
	}
	return account, ""
}

// mayUseRuntime decides with the token of the account, and, for an
// administrator whose network logon UAC filtered, with its full token too.
func (s *System) mayUseRuntime(token windows.Token) error {
	tokens := []windows.Token{token}
	if linked, err := token.GetLinkedToken(); err == nil {
		defer linked.Close()
		tokens = append(tokens, linked)
	}
	var errs []error
	for _, t := range tokens {
		err := s.tokenMayUseRuntime(t)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *System) tokenMayUseRuntime(token windows.Token) error {
	// AccessCheck and CheckTokenMembership take an impersonation token;
	// identification level is enough for both
	var imp windows.Token
	if err := windows.DuplicateTokenEx(token, 0, nil, windows.SecurityIdentification, windows.TokenImpersonation, &imp); err != nil {
		return fmt.Errorf("duplicate token: %w", err)
	}
	defer imp.Close()

	if s.pipe == "" {
		if isMember(imp, windows.WinBuiltinAdministratorsSid) || isProcessUser(imp) {
			return nil
		}
		return errors.New("the runtime has no named pipe; only Administrators and the account ContainerUI runs as may sign in")
	}

	const info = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	sd, err := windows.GetNamedSecurityInfo(s.pipe, windows.SE_FILE_OBJECT, info)
	if err == nil {
		ok, err := accessCheck(sd, imp, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE)
		if err != nil {
			return fmt.Errorf("access check on %s: %w", s.pipe, err)
		}
		if !ok {
			return fmt.Errorf("no read/write permission on %s", s.pipe)
		}
		return nil
	}
	// The pipe is not there (runtime stopped) or busy: decide by the groups
	// Docker grants it to.
	s.log.Debug("runtime pipe not readable, checking groups instead", "pipe", s.pipe, "error", err)
	if isMember(imp, windows.WinBuiltinAdministratorsSid) {
		return nil
	}
	if sid, _, _, err := windows.LookupSID("", "docker-users"); err == nil {
		if ok, err := imp.IsMember(sid); err == nil && ok {
			return nil
		}
	}
	return fmt.Errorf("not a member of Administrators or docker-users (%s: %v)", s.pipe, err)
}

func isMember(token windows.Token, well windows.WELL_KNOWN_SID_TYPE) bool {
	sid, err := windows.CreateWellKnownSid(well)
	if err != nil {
		return false
	}
	ok, err := token.IsMember(sid)
	return err == nil && ok
}

// isProcessUser reports whether token belongs to the account this process
// runs as.
func isProcessUser(token windows.Token) bool {
	theirs, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	ours, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return theirs.User.Sid.Equals(ours.User.Sid)
}

var (
	modadvapi32     = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW  = modadvapi32.NewProc("LogonUserW")
	procAccessCheck = modadvapi32.NewProc("AccessCheck")
)

const (
	logon32LogonNetwork    = 3
	logon32ProviderDefault = 0
	fileAllAccess          = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
)

// logonUser checks the password with a network logon, which needs no logon
// right beyond "Access this computer from the network" and loads no profile.
func logonUser(user, domain, password string) (windows.Token, error) {
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return 0, err
	}
	var d *uint16
	if domain != "" {
		if d, err = windows.UTF16PtrFromString(domain); err != nil {
			return 0, err
		}
	}
	p, err := windows.UTF16FromString(password)
	if err != nil {
		return 0, err
	}
	defer clear(p)
	var token windows.Token
	r, _, err := procLogonUserW.Call(
		uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(&p[0])),
		logon32LogonNetwork, logon32ProviderDefault, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return 0, err
	}
	return token, nil
}

type genericMapping struct {
	read, write, execute, all uint32
}

// accessCheck reports whether the security descriptor grants desired to
// the impersonation token, as opening the pipe would decide it.
func accessCheck(sd *windows.SECURITY_DESCRIPTOR, token windows.Token, desired uint32) (bool, error) {
	mapping := genericMapping{
		read:    windows.FILE_GENERIC_READ,
		write:   windows.FILE_GENERIC_WRITE,
		execute: windows.FILE_GENERIC_EXECUTE,
		all:     fileAllAccess,
	}
	var privileges [16]uint32 // PRIVILEGE_SET, which no privilege is asked for
	privilegesLen := uint32(unsafe.Sizeof(privileges))
	var granted uint32
	var status int32
	r, _, err := procAccessCheck.Call(
		uintptr(unsafe.Pointer(sd)), uintptr(token), uintptr(desired), uintptr(unsafe.Pointer(&mapping)),
		uintptr(unsafe.Pointer(&privileges[0])), uintptr(unsafe.Pointer(&privilegesLen)),
		uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return false, err
	}
	return status != 0, nil
}
