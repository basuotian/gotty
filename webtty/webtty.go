package webtty

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

func init() {
	loglevel := os.Getenv("GOTTY_LOG_LEVEL")
	if loglevel == "" {
		loglevel = "info"
	}
	level, err := log.ParseLevel(loglevel)
	if err != nil {
		level = log.InfoLevel
	}
	log.SetFormatter(&log.TextFormatter{})
	log.SetLevel(log.Level(level))
	log.SetReportCaller(true)
}

// WebTTY bridges a PTY slave and its PTY master.
// To support text-based streams and side channel commands such as
// terminal resizing, WebTTY uses an original protocol.
type WebTTY struct {
	// PTY Master, which probably a connection to browser
	masterConn Master
	// PTY Slave
	slave Slave

	windowTitle []byte
	permitWrite bool
	columns     int
	rows        int
	reconnect   int // in seconds
	masterPrefs []byte

	bufferSize int
	writeMutex sync.Mutex

	auditBuffer         []byte
	auditUser           string
	waitForAutocomplete bool
}

// New creates a new instance of WebTTY.
// masterConn is a connection to the PTY master,
// typically it's a websocket connection to a client.
// slave is a PTY slave such as a local command with a PTY.
func New(masterConn Master, slave Slave, options ...Option) (*WebTTY, error) {
	wt := &WebTTY{
		masterConn: masterConn,
		slave:      slave,

		permitWrite: false,
		columns:     0,
		rows:        0,

		bufferSize: 1024,
	}

	for _, option := range options {
		option(wt)
	}

	return wt, nil
}

// Run starts the main process of the WebTTY.
// This method blocks until the context is canceled.
// Note that the master and slave are left intact even
// after the context is canceled. Closing them is caller's
// responsibility.
// If the connection to one end gets closed, returns ErrSlaveClosed or ErrMasterClosed.
func (wt *WebTTY) Run(ctx context.Context) error {
	err := wt.sendInitializeMessage()
	if err != nil {
		return errors.Wrapf(err, "failed to send initializing message")
	}

	errs := make(chan error, 2)

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				n, err := wt.slave.Read(buffer)
				if err != nil {
					return ErrSlaveClosed
				}

				err = wt.handleSlaveReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				n, err := wt.masterConn.Read(buffer)
				if err != nil {
					return ErrMasterClosed
				}

				err = wt.handleMasterReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}

	return err
}

func (wt *WebTTY) sendInitializeMessage() error {
	err := wt.masterWrite(append([]byte{SetWindowTitle}, wt.windowTitle...))
	if err != nil {
		return errors.Wrapf(err, "failed to send window title")
	}

	if wt.reconnect > 0 {
		reconnect, _ := json.Marshal(wt.reconnect)
		err := wt.masterWrite(append([]byte{SetReconnect}, reconnect...))
		if err != nil {
			return errors.Wrapf(err, "failed to set reconnect")
		}
	}

	if wt.masterPrefs != nil {
		err := wt.masterWrite(append([]byte{SetPreferences}, wt.masterPrefs...))
		if err != nil {
			return errors.Wrapf(err, "failed to set preferences")
		}
	}

	return nil
}

func (wt *WebTTY) handleSlaveReadEvent(data []byte) error {
	wt.audit("send", data)
	safeMessage := base64.StdEncoding.EncodeToString(data)
	err := wt.masterWrite(append([]byte{Output}, []byte(safeMessage)...))
	if err != nil {
		return errors.Wrapf(err, "failed to send message to master")
	}

	return nil
}

func (wt *WebTTY) masterWrite(data []byte) error {
	wt.writeMutex.Lock()
	defer wt.writeMutex.Unlock()

	_, err := wt.masterConn.Write(data)
	if err != nil {
		return errors.Wrapf(err, "failed to write to master")
	}

	return nil
}

func (wt *WebTTY) handleMasterReadEvent(data []byte) error {
	if len(data) == 0 {
		return errors.New("unexpected zero length read from master")
	}
	wt.audit("recive", data[1:])
	switch data[0] {
	case Input:
		if !wt.permitWrite {
			return nil
		}

		if len(data) <= 1 {
			return nil
		}

		_, err := wt.slave.Write(data[1:])
		if err != nil {
			return errors.Wrapf(err, "failed to write received data to slave")
		}

	case Ping:
		err := wt.masterWrite([]byte{Pong})
		if err != nil {
			return errors.Wrapf(err, "failed to return Pong message to master")
		}

	case ResizeTerminal:
		if wt.columns != 0 && wt.rows != 0 {
			break
		}

		if len(data) <= 1 {
			return errors.New("received malformed remote command for terminal resize: empty payload")
		}

		var args argResizeTerminal
		err := json.Unmarshal(data[1:], &args)
		if err != nil {
			return errors.Wrapf(err, "received malformed data for terminal resize")
		}
		rows := wt.rows
		if rows == 0 {
			rows = int(args.Rows)
		}

		columns := wt.columns
		if columns == 0 {
			columns = int(args.Columns)
		}

		wt.slave.ResizeTerminal(columns, rows)
	default:
		return errors.Errorf("unknown message type `%c`", data[0])
	}

	return nil
}

func (wt *WebTTY) audit(action string, msg []byte) {
	if !filterASCII(action, msg) {
		return
	}
	if action == "send" {
		if wt.waitForAutocomplete {
			wt.waitForAutocomplete = false
			wt.auditBuffer = append(wt.auditBuffer, msg...)
		}

		if len(msg) > 1 && msg[:len(msg)][0] != 35 {
			log.WithFields(log.Fields{
				"time": time.Now(),
				"user": wt.auditUser,
			}).Debug("ASCII返回:", asciiToString(msg))
			// output := strings.Replace(string(msg), "sh-4.3#", "", -1)
			// log.WithFields(log.Fields{
			// 	"time": time.Now(),
			// 	"user": wt.auditUser,
			// }).Info("msg=", output)
		}
	} else if action == "recive" {
		if len(msg) > 0 {
			log.Debug(time.Now(), wt.auditUser, "--- ASCII返回:", asciiToString(msg))
			for i, s := range msg {
				if s == 9 {
					// tab
					wt.waitForAutocomplete = true
					continue
				}
				if s == 8 {
					wt.auditBuffer = wt.auditBuffer[:len(wt.auditBuffer)]
					continue
				}
				if s == 13 {
					if len(wt.auditBuffer) > 0 && i == len(msg)-1 {
						output := strings.Replace(string(wt.auditBuffer), "sh-4.3#", "", -1)
						log.WithFields(log.Fields{
							"time": time.Now(),
							"user": wt.auditUser,
						}).Info("msg=", output)
						wt.auditBuffer = []byte{}
						continue
					}
					if i == 0 {
						log.Debug("---- 开头返回换行，跳过")
						return
					}

				} else {
					log.Debug("---- 单个ASCII返回: ", s)
					wt.auditBuffer = append(wt.auditBuffer, s)
				}
			}
		}
	}
}

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

func filterASCII(action string, msg []byte) bool {
	if len(msg) > 1 && msg[0] == 13 && msg[1] == 10 && msg[len(msg)-1] == 32 && msg[len(msg)-2] == 35 {
		// log.Debug("---CR LF sh-4.3#---，不审计")
		return false
	}
	if len(msg) > 1 && msg[0] == 115 && msg[1] == 104 && msg[len(msg)-1] == 32 && msg[len(msg)-2] == 35 {
		// log.Debug("---sh-4.3#---，不审计")
		return false
	}
	if len(msg) == 2 && msg[0] == 13 && msg[1] == 10 {
		// log.Debug("---CR LF---，不审计")
		return false
	}
	if action == "send" && len(msg) == 1 && msg[0] == 13 {
		// log.Debug("---CR---，不审计")
		return false
	}

	return true
}

func asciiToString(msg []byte) string {
	s := ""
	for _, a := range msg {
		as := asciiControlChars[int(a)]
		if as != "" {
			s += "*" + as + "*"
		} else {
			s += string(a)
		}
	}
	return s
}
