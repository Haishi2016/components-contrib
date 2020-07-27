// ------------------------------------------------------------
// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
// ------------------------------------------------------------

package rethinkdb

import (
	"encoding/json"
	"io/ioutil"
	"strconv"
	"strings"
	"time"

	r "github.com/dancannon/gorethink"
	"github.com/dapr/components-contrib/state"
	"github.com/dapr/dapr/pkg/logger"
	"github.com/pkg/errors"
)

const (
	stateTableName          = "daprstate"
	stateTablePKName        = "id"
	stateArchiveTableName   = "daprstate_archive"
	stateArchiveTablePKName = "key"
)

// RethinkDB is a state store implementation for RethinkDB.
type RethinkDB struct {
	session *r.Session
	config  *StateConfig
	logger  logger.Logger
}

// StateConfig represents configuration for RethinkDB
type StateConfig struct {
	r.ConnectOpts
	Archive bool `json:"archive"`
}

// StateRecord represents a single state record
type StateRecord struct {
	ID   string      `json:"id" rethinkdb:"id"`
	TS   int64       `json:"timestamp" rethinkdb:"timestamp"`
	Hash string      `json:"hash,omitempty" rethinkdb:"hash,omitempty"`
	Data interface{} `json:"data,omitempty" rethinkdb:"data,omitempty"`
}

// NewRethinkDBStateStore returns a new RethinkDB state store.
func NewRethinkDBStateStore(logger logger.Logger) *RethinkDB {
	return &RethinkDB{logger: logger}
}

// Init parses metadata, initializes the RethinkDB client, and ensures the state table exists
func (s *RethinkDB) Init(metadata state.Metadata) error {
	r.Log.Out = ioutil.Discard
	r.SetTags("rethinkdb", "json")
	cfg, err := metadataToConfig(metadata.Properties, s.logger)
	if err != nil {
		return errors.Wrap(err, "unable to parse metadata properties")
	}

	// in case someone runs Init multiple times
	if s.session != nil && s.session.IsConnected() {
		s.session.Close()
	}
	ses, err := r.Connect(cfg.ConnectOpts)
	if err != nil {
		return errors.Wrap(err, "error connecting to the database")
	}

	s.session = ses
	s.config = cfg

	// check if table already exists
	c, err := r.DB(s.config.Database).TableList().Run(s.session)
	if err != nil {
		return errors.Wrap(err, "error checking for state table existence in DB")
	}

	if c == nil {
		return errors.Wrap(err, "invalid database response, cursor required")
	}
	defer c.Close()

	var list []string
	err = c.All(&list)
	if err != nil {
		return errors.Wrap(err, "invalid database responsewhile listing tables")
	}

	if !tableExists(list, stateTableName) {
		_, err = r.DB(s.config.Database).TableCreate(stateTableName, r.TableCreateOpts{
			PrimaryKey: stateTablePKName,
		}).RunWrite(s.session)
		if err != nil {
			return errors.Wrap(err, "error creating state table in DB")
		}
	}

	if s.config.Archive && !tableExists(list, stateArchiveTableName) {
		// create archive table with autokey to preserve state id
		_, err = r.DB(s.config.Database).TableCreate(stateArchiveTableName,
			r.TableCreateOpts{PrimaryKey: stateArchiveTablePKName}).RunWrite(s.session)
		if err != nil {
			return errors.Wrap(err, "error creating state archive table in DB")
		}
		// index archive table for id and timestamp
		_, err = r.DB(s.config.Database).Table(stateArchiveTableName).
			IndexCreateFunc("state_index", func(row r.Term) interface{} {
				return []interface{}{row.Field("id"), row.Field("timestamp")}
			}).RunWrite(s.session)
		if err != nil {
			return errors.Wrap(err, "error creating state archive index in DB")
		}
	}

	return nil
}

func (s *RethinkDB) checkConnection() error {
	if s.session == nil {
		return errors.New("state store has not been initialized")
	}
	if !s.session.IsConnected() {
		if err := s.session.Reconnect(r.CloseOpts{NoReplyWait: true}); err != nil {
			return errors.Wrap(err, "error reconnecting to the database")
		}
	}
	return nil
}

func tableExists(arr []string, table string) bool {
	for _, a := range arr {
		if a == table {
			return true
		}
	}
	return false
}

// Get retrieves a RethinkDB KV item
func (s *RethinkDB) Get(req *state.GetRequest) (*state.GetResponse, error) {
	if req == nil || req.Key == "" {
		return nil, errors.New("invalid state request, missing key")
	}

	c, err := r.Table(stateTableName).Get(req.Key).Run(s.session)
	if err != nil {
		return nil, errors.Wrap(err, "error getting record from the database")
	}

	if c == nil || c.IsNil() {
		return &state.GetResponse{}, nil
	}

	if c != nil {
		defer c.Close()
	}

	var doc StateRecord
	err = c.One(&doc)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing database content")
	}

	resp := &state.GetResponse{ETag: doc.Hash}
	b, ok := doc.Data.([]byte)
	if ok {
		resp.Data = b
	} else {
		data, err := json.Marshal(doc.Data)
		if err != nil {
			return nil, errors.New("error serializing data from database")
		}
		resp.Data = data
	}
	return resp, nil
}

// Set saves a state KV item
func (s *RethinkDB) Set(req *state.SetRequest) error {
	if req == nil || req.Key == "" || req.Value == nil {
		return errors.New("invalid state request, key and value required")
	}
	return s.BulkSet([]state.SetRequest{*req})
}

// BulkSet performs a bulk save operation
func (s *RethinkDB) BulkSet(req []state.SetRequest) error {
	docs := make([]*StateRecord, len(req))
	for i, v := range req {
		docs[i] = &StateRecord{
			ID:   v.Key,
			TS:   time.Now().UTC().UnixNano(),
			Hash: v.ETag,
			Data: v.Value,
		}
	}

	resp, err := r.Table(stateTableName).Insert(docs, r.InsertOpts{
		Conflict:      "replace",
		ReturnChanges: true,
	}).RunWrite(s.session)
	if err != nil {
		return errors.Wrap(err, "error saving records to the database")
	}

	if s.config.Archive && len(resp.Changes) > 0 {
		s.archive(resp.Changes)
	}

	return nil
}

func (s *RethinkDB) archive(changes []r.ChangeResponse) error {
	list := make([]map[string]interface{}, 0)
	for _, c := range changes {
		if c.NewValue != nil {
			record, ok := c.NewValue.(map[string]interface{})
			if !ok {
				s.logger.Infof("invalid state DB change type: %T", c.NewValue)
				continue
			}
			list = append(list, record)
		}
	}
	if len(list) > 0 {
		_, err := r.Table(stateArchiveTableName).Insert(list).RunWrite(s.session)
		if err != nil {
			return errors.Wrap(err, "error archiving records to the database")
		}
	}
	return nil
}

// Delete performes a RethinkDB KV delete operation
func (s *RethinkDB) Delete(req *state.DeleteRequest) error {
	if req == nil || req.Key == "" {
		return errors.New("invalid request, missing key")
	}
	return s.BulkDelete([]state.DeleteRequest{*req})
}

// BulkDelete performs a bulk delete operation
func (s *RethinkDB) BulkDelete(req []state.DeleteRequest) error {
	list := make([]string, 0)
	for _, d := range req {
		list = append(list, d.Key)
	}

	c, err := r.Table(stateTableName).GetAll(r.Args(list)).Delete().Run(s.session)
	if err != nil {
		return errors.Wrap(err, "error deleting record from the database")
	}
	defer c.Close()
	return nil
}

// Multi performs multiple operations
func (s *RethinkDB) Multi(reqs []state.TransactionalRequest) error {
	upserts := make([]state.SetRequest, 0)
	deletes := make([]state.DeleteRequest, 0)

	for _, v := range reqs {
		switch v.Operation {
		case state.Upsert:
			r, ok := v.Request.(state.SetRequest)
			if !ok {
				return errors.Errorf("invalid request type (expected SetRequest, got %t)", v.Request)
			}
			upserts = append(upserts, r)
		case state.Delete:
			r, ok := v.Request.(state.DeleteRequest)
			if !ok {
				return errors.Errorf("invalid request type (expected DeleteRequest, got %t)", v.Request)
			}
			deletes = append(deletes, r)
		default:
			return errors.Errorf("invalid operation type: %s", v.Operation)
		}
	}

	// best effort, no transacts supported
	if err := s.BulkSet(upserts); err != nil {
		return errors.Wrap(err, "error saving records to the database")
	}

	if err := s.BulkDelete(deletes); err != nil {
		return errors.Wrap(err, "error saving records to the database")
	}

	return nil
}

func metadataToConfig(cfg map[string]string, logger logger.Logger) (*StateConfig, error) {
	c := StateConfig{}
	for k, v := range cfg {
		switch k {
		case "address": //string
			c.Address = v
		case "addresses": // []string
			c.Addresses = strings.Split(v, ",")
		case "database": //string
			c.Database = v
		case "username": //string
			c.Username = v
		case "password": //string
			c.Password = v
		case "authkey": //string
			c.AuthKey = v
		case "timeout": //time.Duration
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid timeout format: %v", v)
			}
			c.Timeout = d
		case "write_timeout": //time.Duration
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid write timeout format: %v", v)
			}
			c.WriteTimeout = d
		case "read_timeout": //time.Duration
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid read timeout format: %v", v)
			}
			c.ReadTimeout = d
		case "keep_alive_timeout": //time.Duration
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid keep alive timeout format: %v", v)
			}
			c.KeepAlivePeriod = d
		case "initial_cap": //int
			i, err := strconv.Atoi(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid keep initial cap format: %v", v)
			}
			c.InitialCap = i
		case "max_open": //int
			i, err := strconv.Atoi(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid keep max open format: %v", v)
			}
			c.MaxOpen = i
		case "discover_hosts": //bool
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid discover hosts format: %v", v)
			}
			c.DiscoverHosts = b
		case "use-open-tracing": //bool
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid use open tracing format: %v", v)
			}
			c.UseOpentracing = b
		case "archive": //bool
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid use open tracing format: %v", v)
			}
			c.Archive = b
		case "max_idle": //int
			i, err := strconv.Atoi(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid keep max idle format: %v", v)
			}
			c.InitialCap = i
		default:
			logger.Infof("unrecognized metadata: %s", k)
		}
	}

	return &c, nil
}
