package mtproto

import (
	cryptoRand "crypto/rand"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ansel1/merry"
	"golang.org/x/net/proxy"
	"golang.org/x/sync/semaphore"
)

//go:generate go run scheme/generate_tl_schema.go 126 scheme/tl-schema-126.tl tl_schema.go
//go:generate gofmt -w tl_schema.go

const ROUTINES_COUNT = 4

var ErrNoSessionData = merry.New("no session data")

type SessionInfo struct {
	DcID        int32  `json:"dc_id"`
	AuthKey     []byte `json:"auth_key"`
	AuthKeyHash []byte `json:"auth_key_hash"`
	ServerSalt  int64  `json:"server_salt"`
	Addr        string `json:"addr"`
	sessionId   int64
}

type AppConfig struct {
	AppID          int32
	AppHash        string
	AppVersion     string
	DeviceModel    string
	SystemVersion  string
	SystemLangCode string
	LangPack       string
	LangCode       string
}

type MTProto struct {
	sessionStore SessionStore
	session      *SessionInfo
	appCfg       *AppConfig
	connDialer   proxy.Dialer
	conn         net.Conn
	log          Logger

	// Two queues here.
	// First (external) has limited size and contains external requests.
	// Second (internal) is unlimited. Special goroutine transfers messages
	// from external to internal queue while len(interbal) < cap(external).
	// This allows throttling (as same as single limited queue).
	// And this allow to safely (without locks) return any failed (due to
	// network probles for example) messages back to internal queue and retry later.
	extSendQueue chan *packetToSend //external
	sendQueue    chan *packetToSend //internal

	routinesStop chan struct{}
	routinesWG   sync.WaitGroup

	mutex            *sync.Mutex
	connectSemaphore *semaphore.Weighted
	reconnSemaphore  *semaphore.Weighted

	encryptionReady    bool
	lastSeqNo          int32
	msgsByID           map[int64]*packetToSend
	seqNo              int32
	msgId              int64
	handleEvent        func(TL)
	handleReconnection func() error

	dcOptions []*TL_dcOption
}

type packetToSend struct {
	msgID   int64
	seqNo   int32
	msg     TL
	resp    chan TL
	needAck bool
}

func newPacket(msg TL, resp chan TL) *packetToSend {
	return &packetToSend{msg: msg, resp: resp}
}

type MTParams struct {
	LogHandler LogHandler
	AppID      int32
	AppHash    string
	AppConfig  *AppConfig
	ConnDialer proxy.Dialer
	SessStore  SessionStore
	Session    *SessionInfo
}

func NewMTProto(appID int32, appHash string) *MTProto {
	return NewMTProtoExt(MTParams{AppID: appID, AppHash: appHash})
}

func NewMTProtoExt(params MTParams) *MTProto {
	if params.LogHandler == nil {
		params.LogHandler = &SimpleLogHandler{}
	}

	if params.AppConfig == nil {
		params.AppConfig = &AppConfig{
			AppID:          0,
			AppHash:        "",
			AppVersion:     "0.0.1",
			DeviceModel:    "Unknown",
			SystemVersion:  runtime.GOOS + "/" + runtime.GOARCH,
			SystemLangCode: "en",
			LangPack:       "",
			LangCode:       "en",
		}
	}

	if params.AppID != 0 {
		params.AppConfig.AppID = params.AppID
	}
	if params.AppHash != "" {
		params.AppConfig.AppHash = params.AppHash
	}

	if params.ConnDialer == nil {
		params.ConnDialer = &net.Dialer{}
	}

	if params.SessStore == nil {
		var exPath string
		ex, err := os.Executable()
		if err != nil {
			Logger{params.LogHandler}.Error(err, "failed to get executable file path")
			exPath = "."
		} else {
			exPath = filepath.Dir(ex)
		}
		params.SessStore = &SessFileStore{exPath + "/session.json"}
	}

	m := &MTProto{
		sessionStore: params.SessStore,
		session:      params.Session,
		connDialer:   params.ConnDialer,
		appCfg:       params.AppConfig,
		log:          Logger{params.LogHandler},

		extSendQueue: make(chan *packetToSend, 64),
		sendQueue:    make(chan *packetToSend, 1024),
		routinesStop: make(chan struct{}, ROUTINES_COUNT),

		msgsByID: make(map[int64]*packetToSend),
		mutex:    &sync.Mutex{},

		connectSemaphore: semaphore.NewWeighted(1),
		reconnSemaphore:  semaphore.NewWeighted(1),
	}
	go m.debugRoutine()
	return m
}

func (m *MTProto) InitSessAndConnect() error {
	if err := m.InitSession(false); err != nil {
		return merry.Wrap(err)
	}
	if err := m.Connect(); err != nil {
		return merry.Wrap(err)
	}
	return nil
}

func (m *MTProto) InitSession(sessEncrIsReady bool) error {
	if m.session == nil {
		m.session = &SessionInfo{}
		err := m.sessionStore.Load(m.session)
		if merry.Is(err, ErrNoSessionData) { //no data
			m.session.Addr = "149.154.167.50:443" //"149.154.167.40"
			m.encryptionReady = false
		} else if err == nil { //got saved session
			m.encryptionReady = true
		} else {
			return merry.Wrap(err)
		}
	} else {
		m.encryptionReady = sessEncrIsReady
	}

	rand.Seed(time.Now().UnixNano())
	m.session.sessionId = rand.Int63()
	return nil
}

func (m *MTProto) CopySession() *SessionInfo {
	sess := *m.session
	return &sess
}

func (m *MTProto) SaveSessionLogged() {
	if err := m.sessionStore.Save(m.session); err != nil {
		m.log.Error(err, "failed to save session data")
	}
}

func (m *MTProto) DCAddr(dcID int32, ipv6 bool) (string, bool) {
	for _, o := range m.dcOptions {
		if o.ID == dcID && o.Ipv6 == ipv6 && !o.Cdn {
			return fmt.Sprintf("%s:%d", o.IpAddress, o.Port), true
		}
	}
	return "", false
}

func (m *MTProto) SetEventsHandler(handler func(TL)) {
	m.handleEvent = handler
}

func (m *MTProto) SetReconnectionHandler(handler func() error) {
	m.handleReconnection = handler
}

func (m *MTProto) initConection() error {
	m.log.Info("connecting to DC %d (%s)...", m.session.DcID, m.session.Addr)
	var err error
	m.conn, err = m.connDialer.Dial("tcp", m.session.Addr)
	if err != nil {
		return merry.Wrap(err)
	}
	_, err = m.conn.Write([]byte{0xef})
	if err != nil {
		return merry.Wrap(err)
	}

	// getting new authKey if need
	if !m.encryptionReady {
		if err = m.makeAuthKey(); err != nil {
			return merry.Wrap(err)
		}
		if err := m.sessionStore.Save(m.session); err != nil {
			return merry.Wrap(err)
		}
		m.encryptionReady = true
	}

	// getting connection configs
	m.log.Debug("connecting: getting config...")
	x, err := m.sendAndReadDirect(TL_invokeWithLayer{
		TL_Layer,
		TL_initConnection{
			Flags:          0,
			ApiID:          m.appCfg.AppID,
			DeviceModel:    m.appCfg.DeviceModel,
			SystemVersion:  m.appCfg.SystemVersion,
			AppVersion:     m.appCfg.AppVersion,
			SystemLangCode: m.appCfg.SystemLangCode,
			LangPack:       m.appCfg.LangPack,
			LangCode:       m.appCfg.LangCode,
			Proxy:          nil, //flagged
			Query:          TL_help_getConfig{},
		},
	})
	if err != nil {
		return merry.Wrap(err)
	}
	if cfg, ok := x.(TL_config); ok {
		m.session.DcID = cfg.ThisDc
		for _, v := range cfg.DcOptions {
			v := v.(TL_dcOption)
			m.dcOptions = append(m.dcOptions, &v)
		}
	} else {
		return WrongRespError(x)
	}
	return nil
}
func (m *MTProto) Connect() error {
	if !m.connectSemaphore.TryAcquire(1) {
		m.log.Info("connection already in progress, aborting")
		return nil
	}
	defer m.connectSemaphore.Release(1)

	var err error
	for i := 4; i >= 0; i-- {
		err = m.initConection()
		if err == nil {
			break
		}
		m.log.Error(err, "failed to connect")
		m.log.Info("trying to connect one more time (%d)", i)
		time.Sleep(1)
	}

	// starting goroutines
	m.log.Debug("connecting: starting routines...")
	m.routinesWG.Add(4)
	go m.sendRoutine()
	go m.readRoutine()
	go m.queueTransferRoutine() // straintg messages transfer from external to internal queue
	go m.pingRoutine()          // starting keepalive pinging

	m.log.Info("connected to DC %d (%s)...", m.session.DcID, m.session.Addr)
	return nil
}

func (m *MTProto) reconnectLogged() {
	m.log.Info("reconnecting...")
	if !m.reconnSemaphore.TryAcquire(1) {
		m.log.Info("reconnection already in progress, aborting")
		return
	}
	defer func() { m.reconnSemaphore.Release(1) }()

	for {
		err := m.reconnect(0, true)
		if err == nil {
			return
		}
		m.log.Error(err, "failed to reconnect")
		m.log.Info("retrying in 5 seconds")
		time.Sleep(5 * time.Second)
		// and trying to reconnect again
	}
}

func (m *MTProto) Reconnect() error {
	return m.reconnect(0, true)
}

func (m *MTProto) reconnect(newDcID int32, mayPassToHandler bool) error {
	m.log.Info("reconnecting: DC %d -> %d", m.session.DcID, newDcID)

	// stopping routines
	m.log.Debug("stopping routines...")
	for i := 0; i < ROUTINES_COUNT; i++ {
		m.routinesStop <- struct{}{}
	}

	// closing connection, readRoutine will then fail to read() and will handle stop signal
	if m.conn != nil {
		if err := m.conn.Close(); err != nil && !IsClosedConnErr(err) {
			return merry.Wrap(err)
		}
	}

	// waiting for all routines to stop
	m.log.Debug("waiting for routines...")
	m.routinesWG.Wait()
	m.log.Debug("done stopping routines...")

	// removing unused stop signals (if any)
	for empty := false; !empty; {
		select {
		case <-m.routinesStop:
		default:
			empty = true
		}
	}

	// saving IDs of messages from msgsByID[],
	// some of them may not have been sent, so we'll resend them after reconnection
	pendingIDs := make([]int64, 0, len(m.msgsByID))
	m.mutex.Lock()
	for id := range m.msgsByID {
		pendingIDs = append(pendingIDs, id)
	}
	m.mutex.Unlock()
	m.log.Debug("found %d pending packet(s)", len(pendingIDs))

	if newDcID != 0 {
		// renewing connection
		if newDcID != m.session.DcID {
			m.encryptionReady = false //TODO: export auth here (if authed)
			//https://github.com/sochix/TLSharp/blob/0940d3d982e9c22adac96b6c81a435403802899a/TLSharp.Core/TelegramClient.cs#L84
		}
		newDcAddr, ok := m.DCAddr(newDcID, false)
		if !ok {
			return merry.Errorf("wrong DC number: %d", newDcID)
		}
		m.session.DcID = newDcID
		m.session.Addr = newDcAddr
	}

	if err := m.Connect(); err != nil {
		return merry.Wrap(err)
	}

	// Checking pending messages.
	// 1) some of them may have been answered, so they will not be in msgsByID[]
	// 2) some of them may have been received by TG, but response has not reached us yet
	// 3) some of them may be actually lost and must be resend
	// Here we resend messages both from (2) and (3). But since msgID and seqNo
	// are preserved, TG will ignore doubles from (2). And (3) will finally reach TG.
	if len(pendingIDs) > 0 {
		var packets []*packetToSend
		m.mutex.Lock()
		for _, id := range pendingIDs {
			packet, ok := m.msgsByID[id]
			if ok {
				packets = append(packets, packet)
			}
		}
		m.pushPendingPacketsUnlocked(packets)
		m.mutex.Unlock()
	}

	m.log.Info("reconnected to DC %d (%s)", m.session.DcID, m.session.Addr)

	if mayPassToHandler && m.handleReconnection != nil {
		if err := m.handleReconnection(); err != nil {
			return merry.Wrap(err)
		}
	}

	return nil
}

func (m *MTProto) NewConnection(dcID int32) (*MTProto, error) {
	session := m.CopySession()
	m.log.Info("making new connection to DC %d (current: %d)", dcID, session.DcID)
	isOnSameDC := session.DcID == dcID
	encrIsReady := isOnSameDC
	session.DcID = dcID
	var ok bool
	session.Addr, ok = m.DCAddr(dcID, false)
	if !ok {
		return nil, merry.Errorf("unable find address for DC #%d", dcID)
	}

	newMT := NewMTProtoExt(MTParams{
		AppConfig:  m.appCfg,
		SessStore:  &SessNoopStore{},
		Session:    session,
		LogHandler: m.log.Hnd,
		ConnDialer: m.connDialer,
	})
	if err := newMT.InitSession(encrIsReady); err != nil {
		return nil, merry.Wrap(err)
	}
	if err := newMT.Connect(); err != nil {
		return nil, merry.Wrap(err)
	}

	if !isOnSameDC {
		res := m.SendSync(TL_auth_exportAuthorization{DcID: dcID})
		exported, ok := res.(TL_auth_exportedAuthorization)
		if !ok {
			return nil, merry.New(UnexpectedTL("auth export", res))
		}
		res = newMT.SendSync(TL_auth_importAuthorization{ID: exported.ID, Bytes: exported.Bytes})
		if _, ok := res.(TL_auth_authorization); !ok {
			return nil, merry.New(UnexpectedTL("auth import", res))
		}
	}
	return newMT, nil
}

func (m *MTProto) Send(msg TLReq) chan TL {
	resp := make(chan TL, 1)
	m.extSendQueue <- newPacket(msg, resp)
	return resp
}

func (m *MTProto) SendSync(msg TLReq) TL {
	resp := make(chan TL, 1)
	m.extSendQueue <- newPacket(msg, resp)
	return <-resp
}

func (m *MTProto) SendSyncRetry(
	msg TLReq, failRetryInterval time.Duration,
	floodNumShortRetries int, floodMaxWait time.Duration,
) TL {
	retryNum := -1
	for {
		retryNum += 1
		res := m.SendSync(msg)

		if IsError(res, "RPC_CALL_FAIL") {
			m.log.Warn("got RPC error, retrying in %s", failRetryInterval)
			time.Sleep(failRetryInterval)
			continue
		}

		if floodWait, ok := IsFloodError(res); ok {
			if retryNum < floodNumShortRetries {
				floodWait = time.Second
			}
			if floodWait > floodMaxWait {
				return res
			}
			m.log.Warn("got flood-wait, retrying in %s, retry #%d of %d short",
				floodWait, retryNum, floodNumShortRetries)
			time.Sleep(floodWait)
			continue
		}

		return res
	}
}

// Must be called only when sendRoutine and recvRoutine are stopped!
func (m *MTProto) sendAndReadDirect(msg TLReq) (TL, error) {
	resp := make(chan TL, 1)
	packet := newPacket(msg, resp)
	err := m.send(packet)
	if err != nil {
		return nil, merry.Wrap(err)
	}

	// small local version or sendRoutine: just sends data and passes error (if any) outside
	stopSend := make(chan struct{})
	stopSendDone := make(chan struct{})
	sendErr := make(chan error)
	go func() {
		for {
			select {
			case <-stopSend:
				close(stopSendDone)
				return
			case x := <-m.sendQueue:
				m.log.Debug("direct send: sending: %#v", x)
				if err := m.send(x); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	defer func() {
		close(stopSend)
		<-stopSendDone
		m.log.Debug("direct send: done")
	}()

	// small version of readRoutine: just reads, processes and checks for error from sending
	for {
		data, err := m.read()
		if err != nil {
			m.clearPacketData(packet.msgID)
			return nil, merry.Wrap(err)
		}
		m.process(m.msgId, m.seqNo, data, false)
		select {
		case res := <-resp:
			return res, nil
		case err := <-sendErr:
			m.clearPacketData(packet.msgID)
			return nil, err
		default:
			m.log.Debug("direct send: waiting for next packet")
		}
	}
}

type AuthDataProvider interface {
	PhoneNumber() (string, error)
	Code() (string, error)
	Password() (string, error)
}

type ScanfAuthDataProvider struct{}

func (ap ScanfAuthDataProvider) PhoneNumber() (string, error) {
	var phonenumber string
	fmt.Print("Enter phone number: ")
	fmt.Scanf("%s", &phonenumber)
	return phonenumber, nil
}

func (ap ScanfAuthDataProvider) Code() (string, error) {
	var code string
	fmt.Print("Enter code: ")
	fmt.Scanf("%s", &code)
	return code, nil
}

func (ap ScanfAuthDataProvider) Password() (string, error) {
	var passwd string
	fmt.Print("Enter password: ")
	fmt.Scanf("%s", &passwd)
	return passwd, nil
}

func (m *MTProto) Auth(authData AuthDataProvider) error {
	phonenumber, err := authData.PhoneNumber()
	if err != nil {
		return merry.Wrap(err)
	}

	var authSentCode TL_auth_sentCode
	flag := true
	for flag {
		x := m.SendSync(TL_auth_sendCode{
			PhoneNumber: phonenumber,
			ApiID:       m.appCfg.AppID,
			ApiHash:     m.appCfg.AppHash,
			Settings:    TL_codeSettings{Flags: 1, CurrentNumber: true},
		})
		switch x := x.(type) {
		case TL_auth_sentCode:
			authSentCode = x
			flag = false
		case TL_rpc_error:
			if x.ErrorCode != TL_ErrSeeOther {
				return WrongRespError(x)
			}
			var newDc int32
			n, _ := fmt.Sscanf(x.ErrorMessage, "PHONE_MIGRATE_%d", &newDc)
			if n != 1 {
				n, _ := fmt.Sscanf(x.ErrorMessage, "NETWORK_MIGRATE_%d", &newDc)
				if n != 1 {
					n, _ := fmt.Sscanf(x.ErrorMessage, "USER_MIGRATE_%d", &newDc)
					if n != 1 {
						return merry.Errorf("RPC error_string:%s", x.ErrorMessage)
					}
				}
			}

			if err := m.reconnect(newDc, false); err != nil {
				return merry.Wrap(err)
			}
			//TODO: save session here?
		default:
			return WrongRespError(x)
		}
	}

	code, err := authData.Code()
	if err != nil {
		return merry.Wrap(err)
	}

	//if authSentCode.Phone_registered
	x := m.SendSync(TL_auth_signIn{phonenumber, authSentCode.PhoneCodeHash, code})
	if IsError(x, "SESSION_PASSWORD_NEEDED") {
		x = m.SendSync(TL_account_getPassword{})
		accPasswd, ok := x.(TL_account_password)
		if !ok {
			return WrongRespError(x)
		}

		passwd, err := authData.Password()
		if err != nil {
			return merry.Wrap(err)
		}

		algo, ok := accPasswd.CurrentAlgo.(TL_passwordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow)
		if !ok {
			return merry.Errorf("unknown password algo %T, application update is maybe needed to log in",
				accPasswd.CurrentAlgo)
		}
		passwdSRP, err := calcInputCheckPasswordSRP(algo, accPasswd, passwd, cryptoRand.Read, m.log.Debug)
		if err != nil {
			return merry.Wrap(err)
		}
		x = m.SendSync(TL_auth_checkPassword{passwdSRP})
		if _, ok := x.(TL_rpc_error); ok {
			return WrongRespError(x)
		}
	}
	auth, ok := x.(TL_auth_authorization)
	if !ok {
		return merry.Errorf("RPC: %#v", x)
	}
	userSelf := auth.User.(TL_user)
	fmt.Printf("Signed in: id %d name <%s %s>\n", userSelf.ID, userSelf.FirstName, userSelf.LastName)
	return nil
}

func (m *MTProto) popPendingPacketsUnlocked() []*packetToSend {
	packets := make([]*packetToSend, 0, len(m.msgsByID))
	msgs := make([]TL, 0)
	for id, packet := range m.msgsByID {
		delete(m.msgsByID, id)
		packets = append(packets, packet)
		msgs = append(msgs, packet.msg)
	}
	m.log.Debug("popped %d pending packet(s): %#v", len(packets), msgs)
	return packets
}
func (m *MTProto) popPendingPackets() []*packetToSend {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.popPendingPacketsUnlocked()
}
func (m *MTProto) pushPendingPacketsUnlocked(packets []*packetToSend) {
	for _, packet := range packets {
		m.sendQueue <- packet
	}
	m.log.Debug("pushed %d pending packet(s)", len(packets))
}
func (m *MTProto) pushPendingPackets(packets []*packetToSend) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.pushPendingPacketsUnlocked(packets)
}
func (m *MTProto) resendPendingPackets() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	packets := m.popPendingPacketsUnlocked()
	m.pushPendingPacketsUnlocked(packets)
}

func (m *MTProto) GetContacts() error {
	x := m.SendSync(TL_contacts_getContacts{0})
	list, ok := x.(TL_contacts_contacts)
	if !ok {
		return merry.Errorf("RPC: %#v", x)
	}

	contacts := make(map[int32]TL_user)
	for _, v := range list.Users {
		if v, ok := v.(TL_user); ok {
			contacts[v.ID] = v
		}
	}
	fmt.Printf(
		"\033[33m\033[1m%10s    %10s    %-30s    %-20s\033[0m\n",
		"id", "mutual", "name", "username",
	)
	for _, v := range list.Contacts {
		v := v.(TL_contact)
		fmt.Printf(
			"%10d    %10t    %-30s    %-20s\n",
			v.UserID,
			toBool(v.Mutual),
			fmt.Sprintf("%s %s", contacts[v.UserID].FirstName, contacts[v.UserID].LastName),
			contacts[v.UserID].Username,
		)
	}

	return nil
}

/*func (m *MTProto) SendMessage(user_id int32, msg string) error {
	resp := make(chan TL, 1)
	m.sendQueue <- packetToSend{
		TL_messages_sendMessage{
			TL_inputPeerContact{user_id},
			msg,
			rand.Int63(),
		},
		resp,
	}
	x := <-resp
	_, ok := x.(TL_messages_sentMessage)
	if !ok {
		return merry.Errorf("RPC: %#v", x)
	}

	return nil
}*/

func (m *MTProto) pingRoutine() {
	defer func() {
		m.log.Debug("pingRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		case <-time.After(60 * time.Second):
			m.extSendQueue <- newPacket(TL_ping{0xCADACADA}, nil)
		}
	}
}

func (m *MTProto) sendRoutine() {
	defer func() {
		m.log.Debug("sendRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		case x := <-m.sendQueue:
			err := m.send(x)
			if IsClosedConnErr(err) {
				continue //closed connection, should receive stop signal now
			}
			if err != nil {
				m.log.Error(err, "sending failed")
				go m.reconnectLogged()
				return
			}
		}
	}
}

func (m *MTProto) readRoutine() {
	defer func() {
		m.log.Debug("readRoutine done")
		m.routinesWG.Done()
	}()
	for {
		select {
		case <-m.routinesStop:
			return
		default:
		}

		data, err := m.read()
		if IsClosedConnErr(err) {
			continue //closed connection, should receive stop signal now
		}
		if err != nil {
			m.log.Error(err, "reading failed")
			go m.reconnectLogged()
			return
		}
		m.process(m.msgId, m.seqNo, data, true)
	}
}

func (m *MTProto) queueTransferRoutine() {
	defer func() {
		m.log.Debug("queueTransferRoutine done")
		m.routinesWG.Done()
	}()
	for {
		if len(m.sendQueue) < cap(m.extSendQueue) {
			select {
			case <-m.routinesStop:
				return
			case msg := <-m.extSendQueue:
				m.sendQueue <- msg
			}
		} else {
			select {
			case <-m.routinesStop:
				return
			default:
				time.Sleep(10000 * time.Microsecond)
			}
		}
	}
}

// Periodically checks messages in "msgsByID" and warns if they stay there too long
func (m *MTProto) debugRoutine() {
	for {
		m.mutex.Lock()
		count := 0
		for id := range m.msgsByID {
			delta := time.Now().Unix() - (id >> 32)
			if delta > 5 {
				m.log.Warn("msgsByID: #%d: is here for %ds", id, delta)
			}
			count++
		}
		m.mutex.Unlock()
		m.log.Debug("msgsByID: %d total", count)
		time.Sleep(5 * time.Second)
	}
}

func (m *MTProto) clearPacketData(msgID int64) {
	m.mutex.Lock()
	packet, ok := m.msgsByID[msgID]
	if ok {
		if packet.resp != nil {
			close(packet.resp)
		}
		delete(m.msgsByID, msgID)
	}
	m.mutex.Unlock()
}
func (m *MTProto) respAndClearPacketData(msgID int64, response TL) {
	m.mutex.Lock()
	packet, ok := m.msgsByID[msgID]
	if ok {
		if packet.resp == nil {
			m.log.Warn("second response to message #%d %#v", msgID, packet.msg)
		} else {
			packet.resp <- response
			close(packet.resp)
			packet.resp = nil
		}
		delete(m.msgsByID, msgID)
	}
	m.mutex.Unlock()
}

func (m *MTProto) process(msgId int64, seqNo int32, dataTL TL, mayPassToHandler bool) {
	switch data := dataTL.(type) {
	case TL_msg_container:
		for _, v := range data.Items {
			m.process(v.MsgID, v.SeqNo, v.Data, true)
		}

	case TL_bad_server_salt:
		m.session.ServerSalt = data.NewServerSalt
		m.SaveSessionLogged()
		m.resendPendingPackets()

	case TL_bad_msg_notification:
		m.respAndClearPacketData(data.BadMsgID, data)

	case TL_msgs_state_info:
		m.respAndClearPacketData(data.ReqMsgID, data)

	case TL_new_session_created:
		m.session.ServerSalt = data.ServerSalt
		m.SaveSessionLogged()

	case TL_ping:
		m.sendQueue <- newPacket(TL_pong{msgId, data.PingID}, nil)

	case TL_pong:
		// (ignore) TODO

	case TL_msgs_ack:
		m.mutex.Lock()
		for _, id := range data.MsgIds {
			packet, ok := m.msgsByID[id]
			if ok {
				packet.needAck = false
				// if request is not waiting for response, removing it
				if m.msgsByID[id].resp == nil { //TODO: packet.resp
					delete(m.msgsByID, id)
				}
			}
		}
		m.mutex.Unlock()

	case TL_rpc_result:
		m.process(msgId, 0, data.obj, false)
		m.respAndClearPacketData(data.reqMsgID, data.obj)

	default:
		if mayPassToHandler && m.handleEvent != nil {
			go m.handleEvent(dataTL)
		}
	}

	// should acknowledge odd ids
	if (seqNo & 1) == 1 {
		m.sendQueue <- newPacket(TL_msgs_ack{[]int64{msgId}}, nil)
	}
}
