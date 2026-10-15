package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// System verifies Linux system accounts: the password through PAM, and
// that the account may use the container runtime, i.e. connect to its
// socket (root, the socket's owner, or a member of its group such as
// "docker"). Without a socket (rootless podman, fake driver) only root and
// the account ContainerUI runs as are allowed.
type System struct {
	service string
	socket  string
	log     *slog.Logger
}

// NewSystem loads libpam. service is the PAM service (/etc/pam.d/<service>),
// socket the runtime's Unix socket or "".
func NewSystem(service, socket string, log *slog.Logger) (*System, error) {
	if err := loadPAM(); err != nil {
		return nil, err
	}
	if os.Geteuid() != 0 {
		log.Warn("not running as root: PAM can usually only verify the password of the account ContainerUI runs as")
	}
	return &System{service: service, socket: socket, log: log}, nil
}

func (s *System) Verify(username, password string) bool {
	if username == "" || password == "" || len(username) > 256 || strings.ContainsRune(username+password, 0) {
		return false
	}
	if err := pamAuthenticate(s.service, username, password); err != nil {
		s.log.Debug("system login failed", "user", username, "error", err)
		return false
	}
	if err := s.mayUseRuntime(username); err != nil {
		s.log.Warn("system login refused: no access to the container runtime", "user", username, "error", err)
		return false
	}
	return true
}

func (s *System) mayUseRuntime(username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	if s.socket == "" {
		if u.Uid == "0" || u.Uid == strconv.Itoa(os.Getuid()) {
			return nil
		}
		return errors.New("the runtime has no socket; only root and the account ContainerUI runs as may sign in")
	}
	fi, err := os.Stat(s.socket)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: no owner information", s.socket)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return err
	}
	if !socketAllows(u.Uid, gids, SocketOwner{UID: st.Uid, GID: st.Gid, Mode: fi.Mode()}) {
		return fmt.Errorf("no read/write permission on %s", s.socket)
	}
	return nil
}

// PAM through purego, so the Linux build needs no cgo.

const (
	pamSuccess       = 0
	pamBufErr        = 5
	pamConvErr       = 19
	pamPromptEchoOff = 1
	pamPromptEchoOn  = 2
	pamSilent        = 0x8000
	pamDisallowNull  = 0x0001
)

type pamMessage struct {
	style int32
	_     int32
	msg   *byte
}

type pamResponse struct {
	resp    *byte
	retcode int32
	_       int32
}

type pamConv struct {
	conv    uintptr
	appdata unsafe.Pointer
}

var (
	pamOnce sync.Once
	pamErr  error
	// pamMu serializes PAM conversations: the callback reads the
	// credentials of the conversation in progress from pamCur.
	pamMu  sync.Mutex
	pamCur struct{ user, password string }

	pamStart        func(service, user string, conv *pamConv, pamh *unsafe.Pointer) int32
	pamAuth         func(pamh unsafe.Pointer, flags int32) int32
	pamAcctMgmt     func(pamh unsafe.Pointer, flags int32) int32
	pamEnd          func(pamh unsafe.Pointer, status int32) int32
	pamStrerror     func(pamh unsafe.Pointer, errnum int32) string
	cCalloc         func(n, size uintptr) unsafe.Pointer
	cMalloc         func(size uintptr) unsafe.Pointer
	cFree           func(p unsafe.Pointer)
	pamConvCallback uintptr
)

func loadPAM() error {
	pamOnce.Do(func() {
		libpam, err := purego.Dlopen("libpam.so.0", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			pamErr = fmt.Errorf("load libpam.so.0 (install libpam0g / pam): %w", err)
			return
		}
		libc, err := purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			pamErr = fmt.Errorf("load libc.so.6: %w", err)
			return
		}
		purego.RegisterLibFunc(&pamStart, libpam, "pam_start")
		purego.RegisterLibFunc(&pamAuth, libpam, "pam_authenticate")
		purego.RegisterLibFunc(&pamAcctMgmt, libpam, "pam_acct_mgmt")
		purego.RegisterLibFunc(&pamEnd, libpam, "pam_end")
		purego.RegisterLibFunc(&pamStrerror, libpam, "pam_strerror")
		purego.RegisterLibFunc(&cCalloc, libc, "calloc")
		purego.RegisterLibFunc(&cMalloc, libc, "malloc")
		purego.RegisterLibFunc(&cFree, libc, "free")
		pamConvCallback = purego.NewCallback(pamConversation)
	})
	return pamErr
}

// cString copies s into C memory; PAM frees responses with free(3).
func cString(s string) *byte {
	p := cMalloc(uintptr(len(s) + 1))
	if p == nil {
		return nil
	}
	b := unsafe.Slice((*byte)(p), len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return (*byte)(p)
}

// pamConversation answers PAM's prompts: the password for hidden prompts,
// the username for visible ones. Informational messages need no answer.
func pamConversation(num int32, msgs unsafe.Pointer, resp *unsafe.Pointer, _ unsafe.Pointer) int32 {
	if num <= 0 || num > 32 {
		return pamConvErr
	}
	out := cCalloc(uintptr(num), unsafe.Sizeof(pamResponse{}))
	if out == nil {
		return pamBufErr
	}
	responses := unsafe.Slice((*pamResponse)(out), num)
	for i, m := range unsafe.Slice((**pamMessage)(msgs), num) {
		switch m.style {
		case pamPromptEchoOff:
			responses[i].resp = cString(pamCur.password)
		case pamPromptEchoOn:
			responses[i].resp = cString(pamCur.user)
		}
	}
	*resp = out
	return pamSuccess
}

func pamAuthenticate(service, username, password string) error {
	pamMu.Lock()
	defer pamMu.Unlock()
	pamCur.user, pamCur.password = username, password
	defer func() { pamCur.user, pamCur.password = "", "" }()

	conv := (*pamConv)(cCalloc(1, unsafe.Sizeof(pamConv{})))
	if conv == nil {
		return errors.New("pam: out of memory")
	}
	conv.conv = pamConvCallback
	var h unsafe.Pointer
	if rc := pamStart(service, username, conv, &h); rc != pamSuccess {
		cFree(unsafe.Pointer(conv))
		return fmt.Errorf("pam_start: error %d", rc)
	}
	rc := pamAuth(h, pamSilent|pamDisallowNull)
	if rc == pamSuccess {
		rc = pamAcctMgmt(h, pamSilent|pamDisallowNull)
	}
	var err error
	if rc != pamSuccess {
		err = fmt.Errorf("pam: %s", pamStrerror(h, rc))
	}
	pamEnd(h, rc)
	cFree(unsafe.Pointer(conv))
	return err
}
