// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package sql

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"

	"github.com/pkg/errors"

	"github.com/quickfixgo/quickfix"
	"github.com/quickfixgo/quickfix/config"
)

const (
	defaultMessagesTable = "messages"
	defaultSessionsTable = "sessions"
)

type sqlStoreFactory struct {
	settings *quickfix.Settings
}

type sqlStore struct {
	sessionID          quickfix.SessionID
	cache              quickfix.MessageStore
	sqlDriver          string
	sqlDataSourceName  string
	sqlConnMaxLifetime time.Duration
	db                 *sql.DB
	placeholder        placeholderFunc
	messagesTable      string
	sessionsTable      string

	sqlUpdateSeqNums      string
	sqlInsertSession      string
	sqlGetSeqNums         string
	sqlUpdateMessage      string
	sqlInsertMessage      string
	sqlGetMessages        string
	sqlUpdateSession      string
	sqlUpdateSenderSeqNum string
	sqlUpdateTargetSeqNum string
	sqlDeleteMessages     string
}

type placeholderFunc func(int) string

var rePlaceholder = regexp.MustCompile(`\?`)

func sqlString(raw string, placeholder placeholderFunc) string {
	if placeholder == nil {
		return raw
	}
	idx := 0
	return rePlaceholder.ReplaceAllStringFunc(raw, func(_ string) string {
		p := placeholder(idx)
		idx++
		return p
	})
}

func postgresPlaceholder(i int) string {
	return fmt.Sprintf("$%d", i+1)
}

// NewStoreFactory returns a sql-based implementation of MessageStoreFactory.
func NewStoreFactory(settings *quickfix.Settings) quickfix.MessageStoreFactory {
	return sqlStoreFactory{settings: settings}
}

// Create creates a new SQLStore implementation of the MessageStore interface.
func (f sqlStoreFactory) Create(sessionID quickfix.SessionID) (msgStore quickfix.MessageStore, err error) {
	globalSettings := f.settings.GlobalSettings()
	dynamicSessions, _ := globalSettings.BoolSetting(config.DynamicSessions)

	sessionSettings, ok := f.settings.SessionSettings()[sessionID]
	if !ok {
		if dynamicSessions {
			sessionSettings = globalSettings
		} else {
			return nil, fmt.Errorf("unknown session: %v", sessionID)
		}
	}

	sqlDriver, err := sessionSettings.Setting(config.SQLStoreDriver)
	if err != nil {
		return nil, err
	}
	sqlDataSourceName, err := sessionSettings.Setting(config.SQLStoreDataSourceName)
	if err != nil {
		return nil, err
	}

	messagesTableName := defaultMessagesTable
	if name, err := sessionSettings.Setting(config.SQLStoreMessagesTableName); err == nil {
		messagesTableName = name
	}

	sessionsTableName := defaultSessionsTable
	if name, err := sessionSettings.Setting(config.SQLStoreSessionsTableName); err == nil {
		sessionsTableName = name
	}

	sqlConnMaxLifetime := 0 * time.Second
	if sessionSettings.HasSetting(config.SQLStoreConnMaxLifetime) {
		sqlConnMaxLifetime, err = sessionSettings.DurationSetting(config.SQLStoreConnMaxLifetime)
		if err != nil {
			return nil, err
		}
	}

	return newSQLStore(sessionID, sqlDriver, sqlDataSourceName, messagesTableName, sessionsTableName, sqlConnMaxLifetime)
}

func newSQLStore(sessionID quickfix.SessionID, driver, dataSourceName, messagesTableName, sessionsTableName string, connMaxLifetime time.Duration) (store *sqlStore, err error) {

	memStore, memErr := quickfix.NewMemoryStoreFactory().Create(sessionID)
	if memErr != nil {
		err = errors.Wrap(memErr, "cache creation")
		return
	}

	store = &sqlStore{
		sessionID:          sessionID,
		cache:              memStore,
		sqlDriver:          driver,
		sqlDataSourceName:  dataSourceName,
		sqlConnMaxLifetime: connMaxLifetime,
		messagesTable:      messagesTableName,
		sessionsTable:      sessionsTableName,
	}
	if err = store.cache.Reset(); err != nil {
		err = errors.Wrap(err, "cache reset")
		return
	}

	if store.sqlDriver == "postgres" || store.sqlDriver == "pgx" {
		store.placeholder = postgresPlaceholder
	}

	if store.db, err = sql.Open(store.sqlDriver, store.sqlDataSourceName); err != nil {
		return nil, err
	}
	store.db.SetConnMaxLifetime(store.sqlConnMaxLifetime)

	if err = store.db.Ping(); err != nil { // ensure immediate connection
		return nil, err
	}

	store.setSQLStatements()

	if err = store.populateCache(); err != nil {
		return nil, err
	}

	return store, nil
}

func (store *sqlStore) setSQLStatements() {
	idColumns := `beginstring, session_qualifier, sendercompid, sendersubid, senderlocid, targetcompid, targetsubid, targetlocid`
	idPlaceholders := `?,?,?,?,?,?,?,?`
	idWhereClause := `beginstring=? AND session_qualifier=? AND sendercompid=? AND sendersubid=? AND senderlocid=? AND targetcompid=? AND targetsubid=? AND targetlocid=?`

	store.sqlInsertMessage = fmt.Sprintf(`INSERT INTO %s (
		msgseqnum, message, %s) VALUES (?, ?, %s)`,
		store.messagesTable, idColumns, idPlaceholders)

	store.sqlUpdateMessage = fmt.Sprintf(`UPDATE %s SET message=? WHERE %s AND msgseqnum=?`,
		store.messagesTable, idWhereClause)

	store.sqlGetMessages = fmt.Sprintf(`SELECT message FROM %s WHERE %s AND msgseqnum>=? AND msgseqnum<=? ORDER BY msgseqnum`,
		store.messagesTable, idWhereClause)

	store.sqlDeleteMessages = fmt.Sprintf(`DELETE FROM %s WHERE %s`,
		store.messagesTable, idWhereClause)

	store.sqlInsertSession = fmt.Sprintf(`INSERT INTO %s (
		creation_time, incoming_seqnum, outgoing_seqnum, %s) VALUES (?, ?, ?, %s)`,
		store.sessionsTable, idColumns, idPlaceholders)

	store.sqlGetSeqNums = fmt.Sprintf(`SELECT creation_time, incoming_seqnum, outgoing_seqnum FROM %s WHERE %s`,
		store.sessionsTable, idWhereClause)

	store.sqlUpdateSession = fmt.Sprintf(`UPDATE %s SET creation_time=?, incoming_seqnum=?, outgoing_seqnum=? WHERE %s`,
		store.sessionsTable, idWhereClause)

	store.sqlUpdateSenderSeqNum = fmt.Sprintf(`UPDATE %s SET outgoing_seqnum=? WHERE %s`,
		store.sessionsTable, idWhereClause)

	store.sqlUpdateTargetSeqNum = fmt.Sprintf(`UPDATE %s SET incoming_seqnum=? WHERE %s`,
		store.sessionsTable, idWhereClause)

	store.sqlUpdateSeqNums = fmt.Sprintf(`UPDATE %s SET incoming_seqnum=?, outgoing_seqnum=? WHERE %s`,
		store.sessionsTable, idWhereClause)
}

// Reset deletes the store records and sets the seqnums back to 1.
func (store *sqlStore) Reset() error {
	s := store.sessionID
	_, err := store.db.Exec(sqlString(store.sqlDeleteMessages, store.placeholder),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)
	if err != nil {
		return err
	}

	if err = store.cache.Reset(); err != nil {
		return err
	}

	_, err = store.db.Exec(sqlString(store.sqlUpdateSession, store.placeholder),
		store.cache.CreationTime(), store.cache.NextTargetMsgSeqNum(), store.cache.NextSenderMsgSeqNum(),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)

	return err
}

// Refresh reloads the store from the database.
func (store *sqlStore) Refresh() error {
	if err := store.cache.Reset(); err != nil {
		return err
	}
	return store.populateCache()
}

func (store *sqlStore) populateCache() error {
	s := store.sessionID
	var creationTime time.Time
	var incomingSeqNum, outgoingSeqNum int
	row := store.db.QueryRow(sqlString(store.sqlGetSeqNums, store.placeholder),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)

	err := row.Scan(&creationTime, &incomingSeqNum, &outgoingSeqNum)

	// session record found, load it
	if err == nil {
		store.cache.SetCreationTime(creationTime)
		if err = store.cache.SetNextTargetMsgSeqNum(incomingSeqNum); err != nil {
			return errors.Wrap(err, "cache set next target")
		}
		if err = store.cache.SetNextSenderMsgSeqNum(outgoingSeqNum); err != nil {
			return errors.Wrap(err, "cache set next sender")
		}
		return nil
	}

	// fatal error, give up
	if err != sql.ErrNoRows {
		return err
	}

	// session record not found, create it
	_, err = store.db.Exec(sqlString(store.sqlInsertSession, store.placeholder),
		store.cache.CreationTime(),
		store.cache.NextTargetMsgSeqNum(),
		store.cache.NextSenderMsgSeqNum(),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)

	return err
}

// NextSenderMsgSeqNum returns the next MsgSeqNum that will be sent.
func (store *sqlStore) NextSenderMsgSeqNum() int {
	return store.cache.NextSenderMsgSeqNum()
}

// NextTargetMsgSeqNum returns the next MsgSeqNum that should be received.
func (store *sqlStore) NextTargetMsgSeqNum() int {
	return store.cache.NextTargetMsgSeqNum()
}

// SetNextSenderMsgSeqNum sets the next MsgSeqNum that will be sent.
func (store *sqlStore) SetNextSenderMsgSeqNum(next int) error {
	s := store.sessionID
	_, err := store.db.Exec(sqlString(store.sqlUpdateSenderSeqNum, store.placeholder),
		next, s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)
	if err != nil {
		return err
	}
	return store.cache.SetNextSenderMsgSeqNum(next)
}

// SetNextTargetMsgSeqNum sets the next MsgSeqNum that should be received.
func (store *sqlStore) SetNextTargetMsgSeqNum(next int) error {
	s := store.sessionID
	_, err := store.db.Exec(sqlString(store.sqlUpdateTargetSeqNum, store.placeholder),
		next, s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)
	if err != nil {
		return err
	}
	return store.cache.SetNextTargetMsgSeqNum(next)
}

// IncrNextSenderMsgSeqNum increments the next MsgSeqNum that will be sent.
func (store *sqlStore) IncrNextSenderMsgSeqNum() error {
	if err := store.SetNextSenderMsgSeqNum(store.cache.NextSenderMsgSeqNum() + 1); err != nil {
		return errors.Wrap(err, "store next")
	}
	return nil
}

// IncrNextTargetMsgSeqNum increments the next MsgSeqNum that should be received.
func (store *sqlStore) IncrNextTargetMsgSeqNum() error {
	if err := store.SetNextTargetMsgSeqNum(store.cache.NextTargetMsgSeqNum() + 1); err != nil {
		return errors.Wrap(err, "store next")
	}
	return nil
}

// CreationTime returns the creation time of the store.
func (store *sqlStore) CreationTime() time.Time {
	return store.cache.CreationTime()
}

// SetCreationTime is a no-op for SQLStore.
func (store *sqlStore) SetCreationTime(_ time.Time) {
}

func (store *sqlStore) SaveMessage(seqNum int, msg []byte) error {
	s := store.sessionID

	_, err := store.db.Exec(sqlString(store.sqlInsertMessage, store.placeholder),
		seqNum, string(msg),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)

	return err
}

func (store *sqlStore) SaveMessageAndIncrNextSenderMsgSeqNum(seqNum int, msg []byte) error {
	s := store.sessionID

	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(sqlString(store.sqlInsertMessage, store.placeholder),
		seqNum, string(msg),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)
	if err != nil {
		return err
	}

	next := store.cache.NextSenderMsgSeqNum() + 1
	_, err = tx.Exec(sqlString(store.sqlUpdateSenderSeqNum, store.placeholder),
		next, s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID)
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	return store.cache.SetNextSenderMsgSeqNum(next)
}

func (store *sqlStore) IterateMessages(beginSeqNum, endSeqNum int, cb func([]byte) error) error {
	s := store.sessionID
	rows, err := store.db.Query(sqlString(store.sqlGetMessages, store.placeholder),
		s.BeginString, s.Qualifier,
		s.SenderCompID, s.SenderSubID, s.SenderLocationID,
		s.TargetCompID, s.TargetSubID, s.TargetLocationID,
		beginSeqNum, endSeqNum)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var message string
		if err = rows.Scan(&message); err != nil {
			return err
		} else if err = cb([]byte(message)); err != nil {
			return err
		}
	}

	return rows.Err()
}

func (store *sqlStore) GetMessages(beginSeqNum, endSeqNum int) ([][]byte, error) {
	var msgs [][]byte
	err := store.IterateMessages(beginSeqNum, endSeqNum, func(msg []byte) error {
		msgs = append(msgs, msg)
		return nil
	})
	return msgs, err
}

// Close closes the store's database connection.
func (store *sqlStore) Close() error {
	if store.db != nil {
		store.db.Close()
		store.db = nil
	}
	return nil
}
