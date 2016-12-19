package dbs

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/vault/helper/strutil"
	"github.com/lib/pq"
)

type PostgreSQL struct {
	db     *sql.DB
	config ConnectionConfig

	sync.RWMutex
}

func (p *PostgreSQL) Type() string {
	return postgreSQLTypeName
}

func (p *PostgreSQL) Connection() (*sql.DB, error) {
	// Grab the write lock
	p.Lock()
	defer p.Unlock()

	// If we already have a DB, we got it!
	if p.db != nil {
		if err := p.db.Ping(); err == nil {
			return p.db, nil
		}
		// If the ping was unsuccessful, close it and ignore errors as we'll be
		// reestablishing anyways
		p.db.Close()
	}

	// Otherwise, attempt to make connection
	conn := p.config.ConnectionURL

	// Ensure timezone is set to UTC for all the conenctions
	if strings.HasPrefix(conn, "postgres://") || strings.HasPrefix(conn, "postgresql://") {
		if strings.Contains(conn, "?") {
			conn += "&timezone=utc"
		} else {
			conn += "?timezone=utc"
		}
	} else {
		conn += " timezone=utc"
	}

	var err error
	p.db, err = sql.Open("postgres", conn)
	if err != nil {
		return nil, err
	}

	// Set some connection pool settings. We don't need much of this,
	// since the request rate shouldn't be high.
	p.db.SetMaxOpenConns(p.config.MaxOpenConnections)
	p.db.SetMaxIdleConns(p.config.MaxIdleConnections)

	return p.db, nil
}

func (p *PostgreSQL) Close() {
	// Grab the write lock
	p.Lock()
	defer p.Unlock()

	if p.db != nil {
		p.db.Close()
	}

	p.db = nil
}

func (p *PostgreSQL) Reset(config ConnectionConfig) (*sql.DB, error) {
	// Grab the write lock
	p.Lock()
	p.config = config
	p.Unlock()

	p.Close()
	return p.Connection()
}

func (p *PostgreSQL) CreateUser(createStmt, username, password, expiration string) error {
	// Get the connection
	db, err := p.Connection()
	if err != nil {
		return err
	}

	// TODO: This is racey
	// Grab a read lock
	p.RLock()
	defer p.RUnlock()

	// Start a transaction
	//	b.logger.Trace("postgres/pathRoleCreateRead: starting transaction")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		//		b.logger.Trace("postgres/pathRoleCreateRead: rolling back transaction")
		tx.Rollback()
	}()
	// Return the secret

	// Execute each query
	for _, query := range strutil.ParseArbitraryStringSlice(createStmt, ";") {
		query = strings.TrimSpace(query)
		if len(query) == 0 {
			continue
		}

		//		b.logger.Trace("postgres/pathRoleCreateRead: preparing statement")
		stmt, err := tx.Prepare(queryHelper(query, map[string]string{
			"name":       username,
			"password":   password,
			"expiration": expiration,
		}))
		if err != nil {
			return err
		}
		defer stmt.Close()
		//		b.logger.Trace("postgres/pathRoleCreateRead: executing statement")
		if _, err := stmt.Exec(); err != nil {
			return err
		}
	}

	// Commit the transaction

	//	b.logger.Trace("postgres/pathRoleCreateRead: committing transaction")
	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (p *PostgreSQL) RenewUser(username, expiration string) error {
	db, err := p.Connection()
	if err != nil {
		return err
	}
	// TODO: This is Racey
	// Grab the read lock
	p.RLock()
	defer p.RUnlock()

	query := fmt.Sprintf(
		"ALTER ROLE %s VALID UNTIL '%s';",
		pq.QuoteIdentifier(username),
		expiration)

	stmt, err := db.Prepare(query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	if _, err := stmt.Exec(); err != nil {
		return err
	}

	return nil
}

func (p *PostgreSQL) CustomRevokeUser(username, revocationSQL string) error {
	db, err := p.Connection()
	if err != nil {
		return err
	}
	// TODO: this is Racey
	p.RLock()
	defer p.RUnlock()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		tx.Rollback()
	}()

	for _, query := range strutil.ParseArbitraryStringSlice(revocationSQL, ";") {
		query = strings.TrimSpace(query)
		if len(query) == 0 {
			continue
		}

		stmt, err := tx.Prepare(queryHelper(query, map[string]string{
			"name": username,
		}))
		if err != nil {
			return err
		}
		defer stmt.Close()

		if _, err := stmt.Exec(); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (p *PostgreSQL) DefaultRevokeUser(username string) error {
	// Grab the read lock
	p.RLock()
	defer p.RUnlock()

	db, err := p.Connection()
	if err != nil {
		return err
	}

	// Check if the role exists
	var exists bool
	err = db.QueryRow("SELECT exists (SELECT rolname FROM pg_roles WHERE rolname=$1);", username).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if exists == false {
		return nil
	}

	// Query for permissions; we need to revoke permissions before we can drop
	// the role
	// This isn't done in a transaction because even if we fail along the way,
	// we want to remove as much access as possible
	stmt, err := db.Prepare("SELECT DISTINCT table_schema FROM information_schema.role_column_grants WHERE grantee=$1;")
	if err != nil {
		return err
	}
	defer stmt.Close()

	rows, err := stmt.Query(username)
	if err != nil {
		return err
	}
	defer rows.Close()

	const initialNumRevocations = 16
	revocationStmts := make([]string, 0, initialNumRevocations)
	for rows.Next() {
		var schema string
		err = rows.Scan(&schema)
		if err != nil {
			// keep going; remove as many permissions as possible right now
			continue
		}
		revocationStmts = append(revocationStmts, fmt.Sprintf(
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA %s FROM %s;`,
			pq.QuoteIdentifier(schema),
			pq.QuoteIdentifier(username)))

		revocationStmts = append(revocationStmts, fmt.Sprintf(
			`REVOKE USAGE ON SCHEMA %s FROM %s;`,
			pq.QuoteIdentifier(schema),
			pq.QuoteIdentifier(username)))
	}

	// for good measure, revoke all privileges and usage on schema public
	revocationStmts = append(revocationStmts, fmt.Sprintf(
		`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s;`,
		pq.QuoteIdentifier(username)))

	revocationStmts = append(revocationStmts, fmt.Sprintf(
		"REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM %s;",
		pq.QuoteIdentifier(username)))

	revocationStmts = append(revocationStmts, fmt.Sprintf(
		"REVOKE USAGE ON SCHEMA public FROM %s;",
		pq.QuoteIdentifier(username)))

	// get the current database name so we can issue a REVOKE CONNECT for
	// this username
	var dbname sql.NullString
	if err := db.QueryRow("SELECT current_database();").Scan(&dbname); err != nil {
		return err
	}

	if dbname.Valid {
		revocationStmts = append(revocationStmts, fmt.Sprintf(
			`REVOKE CONNECT ON DATABASE %s FROM %s;`,
			pq.QuoteIdentifier(dbname.String),
			pq.QuoteIdentifier(username)))
	}

	// again, here, we do not stop on error, as we want to remove as
	// many permissions as possible right now
	var lastStmtError error
	for _, query := range revocationStmts {
		stmt, err := db.Prepare(query)
		if err != nil {
			lastStmtError = err
			continue
		}
		defer stmt.Close()
		_, err = stmt.Exec()
		if err != nil {
			lastStmtError = err
		}
	}

	// can't drop if not all privileges are revoked
	if rows.Err() != nil {
		return fmt.Errorf("could not generate revocation statements for all rows: %s", rows.Err())
	}
	if lastStmtError != nil {
		return fmt.Errorf("could not perform all revocation statements: %s", lastStmtError)
	}

	// Drop this user
	stmt, err = db.Prepare(fmt.Sprintf(
		`DROP ROLE IF EXISTS %s;`, pq.QuoteIdentifier(username)))
	if err != nil {
		return err
	}
	defer stmt.Close()
	if _, err := stmt.Exec(); err != nil {
		return err
	}

	return nil
}
