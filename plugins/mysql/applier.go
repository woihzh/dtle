package mysql

import (
	"bytes"
	gosql "database/sql"
	"encoding/gob"
	"fmt"
	"time"

	stan "github.com/nats-io/go-nats-streaming"
	"github.com/ngaut/log"
	"github.com/satori/go.uuid"

	uconf "udup/config"
	ubinlog "udup/plugins/mysql/binlog"
	usql "udup/plugins/mysql/sql"
)

const (
	// DefaultConnectWait is the default timeout used for the connect operation
	DefaultConnectWait = 5 * time.Second
)

type Applier struct {
	cfg         *uconf.DriverConfig
	dbs         []*gosql.DB
	singletonDB *gosql.DB
	eventChans  []chan usql.StreamEvent

	eventsChannel chan *ubinlog.BinlogEvent
	currentTx     *ubinlog.Transaction_t
	txChan        chan *ubinlog.Transaction_t

	stanConn stan.Conn
	stanSub  stan.Subscription
}

func NewApplier(cfg *uconf.DriverConfig) *Applier {
	return &Applier{
		cfg:        cfg,
		eventChans: newEventChans(cfg.WorkerCount),
		txChan:     make(chan *ubinlog.Transaction_t, 100),
	}
}

func newEventChans(count int) []chan usql.StreamEvent {
	events := make([]chan usql.StreamEvent, 0, count)
	for i := 0; i < count; i++ {
		events = append(events, make(chan usql.StreamEvent, 1000))
	}

	return events
}

func (a *Applier) InitiateApplier(subject string) error {
	log.Infof("Apply binlog events onto the datasource :%v", a.cfg.ConnCfg.String())

	if err := a.initDBConnections(); err != nil {
		return err
	}

	if err := a.initNatSubClient(); err != nil {
		return err
	}

	if err := a.initiateStreaming(subject); err != nil {
		return err
	}

	// start N applier worker
	for i := 0; i < a.cfg.WorkerCount; i++ {
		go a.startApplierWorker(i, a.dbs[i])
	}

	return nil
}

func (a *Applier) applyTx(db *gosql.DB, transaction *ubinlog.Transaction_t) error {
	/*if err := disableGTIDTxOnline(db); err != nil {
		return err
	}
	defer enableGTIDTxOnline(db)*/
	/*_, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = ON_PERMISSIVE`)
	if err != nil {
		return err
	}

	_, err = usql.ExecNoPrepare(db, `SET GTID_NEXT='ANONYMOUS'`)
	if err != nil {
		return err
	}
	defer func() {
		_, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = ON`)
		if err != nil {
			log.Errorf(err.Error())
		}

		_, err = usql.ExecNoPrepare(db, `SET GTID_NEXT='AUTOMATIC'`)
		if err != nil {
			log.Errorf(err.Error())
		}
	}()*/

	_, err := usql.ExecNoPrepare(db, `SET SQL_LOG_BIN = 0`)
	if err != nil {
		return err
	}

	defer func() {
		_, err := usql.ExecNoPrepare(db, `SET SQL_LOG_BIN = 1`)
		if err != nil {
			log.Errorf(err.Error())
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, query := range transaction.Query {
		if query == "" {
			break
		}

		_, err = tx.Exec(query)
		if err != nil {
			if !usql.IgnoreDDLError(err) {
				return err
			} else {
				log.Warnf("[ignore ddl error][sql]:%s[error]:%v", query, err)
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	return nil
}

func (a *Applier) startApplierWorker(i int, db *gosql.DB) {
	var lastFde, lastSID string
	var lastGNO int64
	for tx := range a.txChan {
		if tx.Fde != "" && lastFde != tx.Fde {
			lastFde = tx.Fde // IMO it would comare the internal pointer first
			_, err := usql.ExecNoPrepare(db, lastFde)
			if err != nil {
				a.cfg.ErrCh <- fmt.Errorf("error applying tx fde: %v", err)
				break
			}
		}

		if lastSID != tx.SID || lastGNO != tx.GNO {
			lastSID = tx.SID
			lastGNO = tx.GNO
			_, err := usql.ExecNoPrepare(db, fmt.Sprintf(`SET GTID_NEXT='%s:%d'`, lastSID, lastGNO))
			if err != nil {
				a.cfg.ErrCh <- err
				break
			}

			txn, err := db.Begin()
			if err != nil {
				a.cfg.ErrCh <- err
				break
			}

			err = txn.Commit()
			if err != nil {
				a.cfg.ErrCh <- err
				break
			}

			_, err = usql.ExecNoPrepare(db, `SET GTID_NEXT='AUTOMATIC'`)
			if err != nil {
				a.cfg.ErrCh <- err
				break
			}

			a.cfg.GtidCh <- fmt.Sprintf("%s:1-%d", lastSID, lastGNO)
		}

		err := a.applyTx(db, tx)
		if err != nil {
			a.cfg.ErrCh <- fmt.Errorf("error applying tx: %v", err)
			break
		}
	}
}

func (a *Applier) initNatSubClient() (err error) {
	sc, err := stan.Connect(uconf.DefaultClusterID, uuid.NewV4().String(), stan.NatsURL(fmt.Sprintf("nats://%s", a.cfg.NatsAddr)), stan.ConnectWait(DefaultConnectWait))
	if err != nil {
		log.Fatalf("Can't connect: %v.\nMake sure a NATS Streaming Server is running at: %s", err, fmt.Sprintf("nats://%s", a.cfg.NatsAddr))
	}
	a.stanConn = sc
	return nil
}

// Decode
func Decode(data []byte, vPtr interface{}) (err error) {
	dec := gob.NewDecoder(bytes.NewBuffer(data))
	err = dec.Decode(vPtr)
	return
}

// initiateStreaming begins treaming of binary log events and registers listeners for such events
func (a *Applier) initiateStreaming(subject string) error {
	sub, err := a.stanConn.Subscribe(subject, func(m *stan.Msg) {
		tx := &ubinlog.Transaction_t{}
		if err := Decode(m.Data, tx); err != nil {
			a.cfg.ErrCh <- fmt.Errorf("Subscribe err:%v", err)
		}
		a.txChan <- tx
	})

	if err != nil {
		return fmt.Errorf("Unexpected error on Subscribe, got %v", err)
	}
	a.stanSub = sub
	return nil
}

func (a *Applier) initDBConnections() (err error) {
	if a.singletonDB, err = usql.CreateDB(a.cfg.ConnCfg); err != nil {
		return err
	}
	a.singletonDB.SetMaxOpenConns(1)
	if err := a.mysqlGTIDMode(); err != nil {
		return err
	}

	if a.dbs, err = usql.CreateDBs(a.cfg.ConnCfg, a.cfg.WorkerCount+1); err != nil {
		return err
	}
	return nil
}

func (a *Applier) mysqlGTIDMode() error {
	query := `SELECT @@GTID_MODE`
	var gtidMode string
	if err := a.singletonDB.QueryRow(query).Scan(&gtidMode); err != nil {
		return err
	}
	if gtidMode != "ON" {
		return fmt.Errorf("must have GTID enabled: %+v", gtidMode)
	}
	return nil
}

func enableGTIDTxOnline(db *gosql.DB) {
	//https://dev.mysql.com/doc/refman/5.7/en/replication-mode-change-online-enable-gtids.html
	/*if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.ENFORCE_GTID_CONSISTENCY = WARN`); err != nil {
		log.Errorf(err.Error())
	}

	if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.ENFORCE_GTID_CONSISTENCY = ON`); err != nil {
		log.Errorf(err.Error())
	}*/

	if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = ON_PERMISSIVE`); err != nil {
		log.Errorf(err.Error())
	}

	/*showCount := `SHOW STATUS LIKE 'ONGOING_ANONYMOUS_TRANSACTION_COUNT'`
	err := usql.QueryRowsMap(db, showCount, func(m usql.RowMap) error {
		if m["Value"].String != "0" {
			return fmt.Errorf("ONGOING_ANONYMOUS_TRANSACTION_COUNT != 0 detected.")
		}
		return nil
	})
	if err != nil {
		return err
	}*/

	if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = ON`); err != nil {
		log.Errorf(err.Error())
	}
}

func disableGTIDTxOnline(db *gosql.DB) error {
	//https://dev.mysql.com/doc/refman/5.7/en/replication-mode-change-online-disable-gtids.html
	if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = ON_PERMISSIVE`); err != nil {
		return err
	}

	if _, err := usql.ExecNoPrepare(db, `SET @@GLOBAL.GTID_MODE = OFF_PERMISSIVE`); err != nil {
		return err
	}
	return nil
}

func (a *Applier) stopFlag() bool {
	return a.cfg.Running
}

func closeEventChans(events []chan usql.StreamEvent) {
	for _, ch := range events {
		close(ch)
	}
}

func (a *Applier) Shutdown() error {
	if !a.stopFlag() {
		return nil
	}
	if err := a.stanSub.Unsubscribe(); err != nil {
		return err
	}
	if err := a.stanConn.Close(); err != nil {
		return err
	}

	closeEventChans(a.eventChans)

	if err := usql.CloseDBs(a.dbs...); err != nil {
		return err
	}
	a.cfg.Running = false
	log.Infof("Closed applier connection.")
	return nil
}