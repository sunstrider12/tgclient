package tgclient

import (
	"mtproto"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/ansel1/merry"
)

type Logger interface {
	Info(args ...interface{})
	Infof(format string, args ...interface{})
	Warning(args ...interface{})
	Warningf(format string, args ...interface{})
	Error(args ...interface{})
	Errorf(format string, args ...interface{})
}

type TGClient struct {
	mt                   *mtproto.MTProto
	updatesState         *mtproto.TL_updates_state
	handleUpdateExternal UpdateHandler
	log                  Logger
	extraData
	Downloader
}

type UpdateHandler func(mtproto.TL)

func NewTGClient(appID int32, appHash string, handleUpdate UpdateHandler, log Logger) (*TGClient, error) {
	ex, err := os.Executable()
	if err != nil {
		return nil, merry.Wrap(err)
	}
	exPath := filepath.Dir(ex)
	sessStore := &mtproto.SessFileStore{exPath + "/tg.session"}

	cfg := &mtproto.AppConfig{
		AppID:          appID,
		AppHash:        appHash,
		AppVersion:     "0.0.1",
		DeviceModel:    "Unknown",
		SystemVersion:  runtime.GOOS + "/" + runtime.GOARCH,
		SystemLangCode: "en",
		LangPack:       "",
		LangCode:       "en",
	}
	return NewTGClientExt(cfg, sessStore, handleUpdate, log)
}

func NewTGClientExt(
	cfg *mtproto.AppConfig, sessStore mtproto.SessionStore,
	handleUpdate UpdateHandler, log Logger,
) (*TGClient, error) {
	mt, err := mtproto.NewMTProtoExt(cfg, sessStore, nil, false)
	if err != nil {
		return nil, merry.Wrap(err)
	}
	if err := mt.Connect(); err != nil {
		return nil, merry.Wrap(err)
	}

	client := &TGClient{
		mt:                   mt,
		updatesState:         &mtproto.TL_updates_state{},
		handleUpdateExternal: handleUpdate,
		log:                  log,
	}
	client.Downloader = *NewDownloader(client)
	client.extraData = *newExtraData(client)

	mt.SetEventsHandler(client.handleEvent)
	for i := 0; i < 4; i++ {
		go client.partsDownloadRoutine()
	}
	return client, nil
}

func (c *TGClient) handleEvent(eventObj mtproto.TL) {
	switch event := eventObj.(type) {
	case mtproto.TL_updatesTooLong:
		//TODO: what?
		// Too many updates, it is necessary to execute updates.getDifference.
		// https://core.telegram.org/constructor/updatesTooLong
		c.log.Warning("[WARN] updates too long")
	case mtproto.TL_updateShort:
		c.updatesState.Date = event.Date
		c.handleUpdate(event.Update)
	case mtproto.TL_updates:
		c.updatesState.Date = event.Date
		c.updatesState.Seq = event.Seq
		c.rememberEventExtraData(event.Users)
		c.rememberEventExtraData(event.Chats)
		for _, u := range event.Updates {
			c.handleUpdate(u)
		}
	case mtproto.TL_updateShortMessage:
		c.updatesState.Date = event.Date
		c.updatesState.Pts = event.Pts
		// update.PtsCount
		c.handleUpdate(event)
	case mtproto.TL_updateShortChatMessage:
		c.updatesState.Date = event.Date
		c.updatesState.Pts = event.Pts
		// update.PtsCount
		c.handleUpdate(event)
	case mtproto.TL_updatesCombined:
		c.updatesState.Date = event.Date
		c.updatesState.Seq = event.Seq
		c.rememberEventExtraData(event.Users)
		c.rememberEventExtraData(event.Chats)
		// update.SeqStart
		for _, u := range event.Updates {
			c.handleUpdate(u)
		}
	case mtproto.TL_updateShortSentMessage:
		c.updatesState.Pts = event.Pts
		// update.PtsCount
		c.handleUpdate(event)
	default:
		c.log.Warning(mtproto.UnexpectedTL("event", eventObj))
	}
}

func (e *TGClient) handleUpdate(obj mtproto.TL) {
	value := reflect.ValueOf(obj).FieldByName("Pts")
	if value != (reflect.Value{}) {
		e.updatesState.Pts = int32(value.Int())
	}
	e.handleUpdateExternal(obj)
}

func (c *TGClient) AuthAndInitEvents(authData mtproto.AuthDataProvider) error {
	for {
		res := c.mt.SendSync(mtproto.TL_updates_getState{})
		if mtproto.IsErrorType(res, mtproto.TL_ErrUnauthorized) { //AUTH_KEY_UNREGISTERED SESSION_REVOKED SESSION_EXPIRED
			if err := c.mt.Auth(authData); err != nil {
				return merry.Wrap(err)
			}
			continue
		}
		_, ok := res.(mtproto.TL_updates_state)
		if !ok {
			return mtproto.WrongRespError(res)
		}
		break
	}
	c.log.Info("Seems authed.")
	return nil
}

func (c *TGClient) SendSync(msg mtproto.TL) mtproto.TL {
	return c.mt.SendSync(msg)
}
