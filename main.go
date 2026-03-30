package main

import (
    "database/sql"
    "log"

    sqlite3 "github.com/mattn/go-sqlite3"
    "github.com/pocketbase/dbx"
    "github.com/pocketbase/pocketbase"

    "github.com/ionutdejeu/pocketbase-vector/plugins"
)

// register a new driver with default PRAGMAs and the same query
// builder implementation as the already existing sqlite3 builder
func init() {
    // initialize default PRAGMAs for each new connection
    sql.Register("pb_sqlite3",
        &sqlite3.SQLiteDriver{
            ConnectHook: func(conn *sqlite3.SQLiteConn) error {
                _, err := conn.Exec(`
                    PRAGMA busy_timeout       = 10000;
                    PRAGMA journal_mode       = WAL;
                    PRAGMA journal_size_limit = 200000000;
                    PRAGMA synchronous        = NORMAL;
                    PRAGMA foreign_keys       = ON;
                    PRAGMA temp_store         = MEMORY;
                    PRAGMA cache_size         = -32000;
                `, nil)

                return err
            },
        },
    )

    dbx.BuilderFuncMap["pb_sqlite3"] = dbx.BuilderFuncMap["sqlite3"]
}

func main() {
    app := pocketbase.NewWithConfig(pocketbase.Config{
        DBConnect: func(dbPath string) (*dbx.DB, error) {
            return dbx.Open("pb_sqlite3", dbPath)
        },
    })

    if err := plugins.VectorStore(app, "vectors"); err != nil {
        log.Fatal(err)
    }

    if err := app.Start(); err != nil {
        log.Fatal(err)
    }
}