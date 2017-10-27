//conversion from dbf to mysql
//version 0.1.0 (probably forever,I'm a kind of conservative in changing the version)
//written by squeeze69

package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"text/template"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/squeeze69/dbf"
)

// default values and other constants
const (
	defaultEngine    = "MyIsam"
	defaultCollation = "utf8_general_ci"
)

//global mysqlurl - see the go lang database/sql package
//sample url: "user:passwordd@(127.0.0.1:3306)/database"
var mysqlurl string

//variuous flags, set by command line, default to false
var verbose, truncate, createtable, dumpcreatetable, insertignore, nobigint, droptable bool

//max number of record to import, defaults to -1 (means no limit)
var maxrecord int

//global variables for --create
var collate = defaultCollation
var engine = defaultEngine

//global variables
var stmt *sql.Stmt

//read profile, actually a fixed position file, first row it's a sql url
func readprofile(prfname string) error {
	s := make([]string, 0, 4)
	f, err := os.Open(prfname)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		s = append(s, scanner.Text())
	}
	mysqlurl = s[0]
	return nil
}

//returns a "CREATE TABLE" string
func createtablestring(table string, collate string, engine string, dbr *dbf.Reader) string {
	var fieldtype string
	//pre allocate, 200 is an arbitrary value
	arf := make([]string, 0, 200)
	fields := dbr.FieldNames()
	for k := range fields {
		dbfld, _ := dbr.FieldInfo(k)
		switch dbfld.Type {
		case 'D':
			fieldtype = "DATE"
		case 'L': //logical
			fieldtype = "CHAR(1)"
		case 'C': //CHAR
			fieldtype = fmt.Sprintf("VARCHAR(%d)", dbfld.Len)
		case 'N': //Numeric could be either Int or fixed point decimal
			if dbfld.DecimalPlaces > 0 {
				//A VARCHAR will do it, +2 it's for sign and decimal sep.
				fieldtype = fmt.Sprintf("VARCHAR(%d)", dbfld.Len+2)
			} else {
				var inttype string
				switch {
				case dbfld.Len < 3:
					inttype = "TINYINT"
				case dbfld.Len >= 3 && dbfld.Len < 5:
					inttype = "SMALLINT"
				case dbfld.Len >= 5 && dbfld.Len < 7:
					inttype = "MEDIUMINT"
				case (dbfld.Len >= 7 && dbfld.Len < 10) || nobigint:
					inttype = "INT"
				case dbfld.Len >= 10:
					inttype = "BIGINT"
				}
				fieldtype = fmt.Sprintf("%s(%d)", inttype, dbfld.Len)
			}
		default:
			fieldtype = fmt.Sprintf("VARCHAR(%d)", dbfld.Len)
		}
		arf = append(arf, fmt.Sprintf("`%s` %s", dbf.Tillzero(dbfld.Name[:]), fieldtype))
	}

	tmpl, err := template.New("table").Parse(
		`CREATE TABLE IF NOT EXISTS {{.Tablename}} (
{{range $i,$e := .Arf}}
{{- if $i}},
{{end}}{{$e}}{{end}}
){{if .Collate}} COLLATE='{{.Collate}}'{{end}}{{if .Engine}} ENGINE='{{.Engine}}'{{end}};`)
	if err != nil {
		log.Fatal(err)
	}
	var str string
	buf := bytes.NewBufferString(str)
	er1 := tmpl.Execute(buf, struct {
		Tablename, Collate, Engine string
		Arf                        []string
	}{Tablename: "`" + table + "`", Collate: collate, Engine: engine, Arf: arf})
	if er1 != nil {
		log.Fatal(er1)
	}
	return buf.String()
}

//Prepare the command line handling
func commandLineSet() {
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.BoolVar(&truncate, "truncate", false, "truncate table before writing")
	flag.BoolVar(&droptable, "drop", false, "drop table before anything")
	flag.BoolVar(&insertignore, "insertignore", false, "use 'INSERT IGNORE' instead of INSERT")
	flag.BoolVar(&nobigint, "nobigint", false, "DON'T use BIGINT type, sometimes fields are over-dimensioned")
	flag.IntVar(&maxrecord, "m", -1, "maximum number of records to read")
	flag.StringVar(&collate, "collate", "utf8_general_ci", "Collate to use with CREATE TABLE (if empty, no collate is specified)")
	flag.StringVar(&engine, "engine", "MyIsam", "Engine to use with CREATE TABLE (if empty, no engine is specified)")
	flag.BoolVar(&createtable, "create", false, "Switch to CREATE TABLE IF NOT EXISTS")
	flag.BoolVar(&dumpcreatetable, "dumpcreatetable", false, "Dump the CREATE TABLE string and exit,no other actions.")
	flag.Parse()

}

//insertRoutine goroutine to insert data
func insertRoutine(ch chan dbf.OrderedRecord, stmt *sql.Stmt) error {
	for i := range ch {
		_, err := stmt.Exec(i...)
		if err != nil {
			panic(err)
		}
	}
	return nil
}

func main() {
	var start = time.Now()
	var qstring string
	var skipped, inserted int
	var insertstatement = "INSERT"

	placeholder := make([]string, 0, 200) //preallocate
	commandLineSet()

	argl := flag.Args()
	if len(argl) < 3 {
		fmt.Println("Usage: dbfgo2mysql [parameters] profile dbffile table")
		flag.PrintDefaults()
		os.Exit(1)
	}
	err := readprofile(argl[0])
	if err != nil {
		log.Fatal("Error!!:", err)
	}

	//open the mysql link
	db, err := sql.Open("mysql", mysqlurl)

	if err != nil {
		log.Fatal("Error!", err)
	}
	defer db.Close()

	inpf, err := os.Open(argl[1])
	if err != nil {
		log.Fatal("dbf file open:", err)
	}
	defer inpf.Close()
	dbfile, err := dbf.NewReader(inpf)
	if err != nil {
		log.Fatal("dbf newreader:", err)
	}
	dbfile.SetFlags(dbf.FlagDateAssql | dbf.FlagSkipWeird | dbf.FlagSkipDeleted)

	//check if the table must be dropped before creation
	if droptable && !dumpcreatetable {
		if verbose {
			fmt.Println("Dropping table:", argl[2])
		}
		if _, erd := db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`;", argl[2])); erd != nil {
			log.Fatal("Dropping:", erd)
		}
	}

	//create table section
	if createtable || dumpcreatetable {
		if verbose {
			fmt.Println("Creating Table: ", argl[2])
		}
		ctstring := createtablestring(argl[2], collate, engine, dbfile)
		if !dumpcreatetable {
			if _, erc := db.Exec(ctstring); erc != nil {
				log.Fatal("CREATE TABLE:", erc, "\n", ctstring)
			}
		}
		if verbose || dumpcreatetable {
			fmt.Println("-- CREATE TABLE:\n", ctstring)
		}
		if dumpcreatetable {
			os.Exit(0)
		}
	}

	//retrieve fields to build the query
	fields := dbfile.FieldNames()
	for i := 0; i < len(fields); i++ {
		placeholder = append(placeholder, "?")
	}
	if truncate && !droptable {
		_, err = db.Exec(fmt.Sprintf("TRUNCATE `%s`;", argl[2]))
		if err != nil {
			log.Fatal("Error truncating:", err)
		}
	}

	//building the code for the prepared statement
	if insertignore {
		insertstatement = "INSERT IGNORE"
	}
	qstring = fmt.Sprintf("%s INTO %s (`%s`) VALUES (%s);", insertstatement, argl[2], strings.Join(fields, "`,`"), strings.Join(placeholder, ","))
	if verbose {
		fmt.Println("QSTRING:", qstring)
	}
	//it's using a prepared statement, much safer and faster
	stmt, err := db.Prepare(qstring)

	if err != nil {
		log.Fatal("Error! Preparing statement:", err, "\n", qstring)
	}
	defer stmt.Close()

	if verbose {
		fmt.Println("Number of dbf records:", dbfile.Length)
	}

	chord := make(chan dbf.OrderedRecord)
	go insertRoutine(chord, stmt)

	for i := 0; i < dbfile.Length; i++ {
		if maxrecord >= 0 && i >= maxrecord {
			break
		}
		rec, err := dbfile.ReadOrdered(i)

		if err == nil {
			if verbose {
				fmt.Println(rec)
			}
			chord <- rec
			inserted++
		} else {
			if _, ok := err.(*dbf.SkipError); ok {
				skipped++
				continue
			}
			log.Fatal("Loop Error: record:", i, " of ", dbfile.Length, " Error:", err)
		}
	}
	close(chord)
	fmt.Printf("Records: Inserted: %d Skipped: %d\nElapsed Time: %s\n",
		inserted, skipped, time.Now().Sub(start))
}
