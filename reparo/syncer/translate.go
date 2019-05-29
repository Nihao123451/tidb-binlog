package syncer

import (
	"github.com/ngaut/log"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb-binlog/pkg/loader"
	pb "github.com/pingcap/tidb-binlog/proto/binlog"
	"github.com/pingcap/tidb/util/codec"
)

func pbBinlogToTxn(binlog *pb.Binlog) (txn *loader.Txn, err error) {
	txn = new(loader.Txn)
	switch binlog.Tp {
	case pb.BinlogType_DDL:
		txn.DDL = new(loader.DDL)
		// for table DDL, pb.Binlog.DdlQuery will be "use <db>; create..."
		txn.DDL.SQL = string(binlog.DdlQuery)
		txn.DDL.Database, txn.DDL.Table, err = parserSchemaTableFromDDL(txn.DDL.SQL)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if len(txn.DDL.Database) == 0 {
			return nil, errors.Errorf("can't parse database name from DDL %s", binlog.DdlQuery)
		}
	case pb.BinlogType_DML:
		data := binlog.DmlData
		for _, event := range data.GetEvents() {
			dml := new(loader.DML)
			dml.Database = event.GetSchemaName()
			dml.Table = event.GetTableName()
			txn.DMLs = append(txn.DMLs, dml)

			switch event.GetTp() {
			case pb.EventType_Insert:
				dml.Tp = loader.InsertDMLType

				cols, args, err := genColsAndArgs(event.Row)
				if err != nil {
					return nil, errors.Trace(err)
				}

				dml.Values = make(map[string]interface{})
				for i := 0; i < len(cols); i++ {
					dml.Values[cols[i]] = args[i]
				}
			case pb.EventType_Update:
				dml.Tp = loader.UpdateDMLType
				dml.Values = make(map[string]interface{})
				dml.OldValues = make(map[string]interface{})

				for _, c := range event.GetRow() {
					col := &pb.Column{}
					err := col.Unmarshal(c)
					if err != nil {
						return nil, errors.Trace(err)
					}

					_, oldDatum, err := codec.DecodeOne(col.Value)
					if err != nil {
						return nil, errors.Trace(err)
					}
					_, newDatum, err := codec.DecodeOne(col.ChangedValue)
					if err != nil {
						return nil, errors.Trace(err)
					}

					tp := col.Tp[0]
					newDatum = formatValue(newDatum, tp)
					newValue := newDatum.GetValue()
					oldDatum = formatValue(oldDatum, tp)
					oldValue := oldDatum.GetValue()

					log.Debugf("%s(%s %v): %v => %v", col.Name, col.MysqlType, tp, oldValue, newValue)

					dml.Values[col.Name] = newValue
					dml.OldValues[col.Name] = oldValue
				}
			case pb.EventType_Delete:
				dml.Tp = loader.DeleteDMLType

				cols, args, err := genColsAndArgs(event.Row)
				if err != nil {
					return nil, errors.Trace(err)
				}

				dml.Values = make(map[string]interface{})
				for i := 0; i < len(cols); i++ {
					dml.Values[cols[i]] = args[i]
				}
			default:
				return nil, errors.Errorf("unknown type: %v", event.GetTp())
			}
		}
	default:
		return nil, errors.Errorf("unknown type: %v", binlog.Tp)
	}

	return
}

func genColsAndArgs(row [][]byte) (cols []string, args []interface{}, err error) {
	cols = make([]string, 0, len(row))
	args = make([]interface{}, 0, len(row))
	for _, c := range row {
		col := &pb.Column{}
		err := col.Unmarshal(c)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		cols = append(cols, col.Name)

		_, val, err := codec.DecodeOne(col.Value)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}

		tp := col.Tp[0]
		val = formatValue(val, tp)
		log.Debugf("%s(%s): %v", col.Name, col.MysqlType, val.GetValue())
		args = append(args, val.GetValue())
	}

	return
}

// parserSchemaTableFromDDL parses ddl query to get schema and table
// ddl like `use test; create table`
func parserSchemaTableFromDDL(ddlQuery string) (schema, table string, err error) {
	stmts, _, err := parser.New().Parse(ddlQuery, "", "")
	if err != nil {
		return "", "", err
	}

	haveUseStmt := false

	for _, stmt := range stmts {
		switch node := stmt.(type) {
		case *ast.UseStmt:
			haveUseStmt = true
			schema = node.DBName
		case *ast.CreateDatabaseStmt:
			schema = node.Name
		case *ast.DropDatabaseStmt:
			schema = node.Name
		case *ast.TruncateTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.CreateIndexStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.CreateTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.DropIndexStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.AlterTableStmt:
			if len(node.Table.Schema.O) != 0 {
				schema = node.Table.Schema.O
			}
			table = node.Table.Name.O
		case *ast.DropTableStmt:
			// FIXME: may drop more than one table in a ddl
			if len(node.Tables[0].Schema.O) != 0 {
				schema = node.Tables[0].Schema.O
			}
			table = node.Tables[0].Name.O
		case *ast.RenameTableStmt:
			if len(node.NewTable.Schema.O) != 0 {
				schema = node.NewTable.Schema.O
			}
			table = node.NewTable.Name.O
		default:
			return "", "", errors.Errorf("unknown ddl type, ddl: %s", ddlQuery)
		}
	}

	if haveUseStmt {
		if len(stmts) != 2 {
			return "", "", errors.Errorf("invalid ddl %s", ddlQuery)
		}
	} else {
		if len(stmts) != 1 {
			return "", "", errors.Errorf("invalid ddl %s", ddlQuery)
		}
	}

	return
}
