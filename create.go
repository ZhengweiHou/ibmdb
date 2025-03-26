package ibmdb

import (
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils"
)

// Create create hook
func Create(config *callbacks.Config) func(db *gorm.DB) {
	// 检查是否支持 RETURNING 子句
	supportReturning := utils.Contains(config.CreateClauses, "RETURNING")

	return func(db *gorm.DB) {
		// 如果数据库操作已经有错误，直接返回
		if db.Error != nil {
			return
		}

		if db.Statement.Schema != nil {
			// 如果不是 Unscoped 模式，添加 Schema 中的 CreateClauses
			if !db.Statement.Unscoped {
				for _, c := range db.Statement.Schema.CreateClauses {
					db.Statement.AddClause(c)
				}
			}

			// 如果支持 RETURNING 并且有默认值字段，添加 RETURNING 子句
			if supportReturning && len(db.Statement.Schema.FieldsWithDefaultDBValue) > 0 {
				if _, ok := db.Statement.Clauses["RETURNING"]; !ok {
					fromColumns := make([]clause.Column, 0, len(db.Statement.Schema.FieldsWithDefaultDBValue))
					for _, field := range db.Statement.Schema.FieldsWithDefaultDBValue {
						fromColumns = append(fromColumns, clause.Column{Name: field.DBName})
					}
					db.Statement.AddClause(clause.Returning{Columns: fromColumns})
				}
			}
		}

		// 如果 SQL 语句为空，构建插入语句
		if db.Statement.SQL.Len() == 0 {
			db.Statement.SQL.Grow(180)
			db.Statement.AddClauseIfNotExists(clause.Insert{})
			db.Statement.AddClause(callbacks.ConvertToCreateValues(db.Statement))

			//if c, ok := db.Statement.Clauses["ON CONFLICT"]; ok {
			//  // TODO db2 merge SQL构建待开发
			//	// 若ON CONFLICT则db2要生成merge语句，其他clause会有干扰，索性就全替换，只由ON CONFLICT clause进行sql拼接
			//	if onConflict, ok := c.Expression.(clause.OnConflict); ok {
			//		db.Statement.Clauses = map[string]clause.Clause{
			//			"ON CONFLICT": clause.Clause{Expression: onConflict},
			//		}
			//	}
			//}

			db.Statement.Build(db.Statement.BuildClauses...)

			if supportReturning && len(db.Statement.Schema.FieldsWithDefaultDBValue) > 0 {
				db.Statement.WriteString(")")
			}
		}

		// 检查是否为 DryRun 模式
		isDryRun := !db.DryRun && db.Error == nil
		if !isDryRun {
			return
		}

		if supportReturning && len(db.Statement.Schema.FieldsWithDefaultDBValue) > 0 {
			// ScanInitialized (1 << 0) - 初始化模式
			//   表示扫描操作是否应该初始化值
			//   当设置此模式时，如果目标是一个结构体，会先将结构体值初始化为零值
			//   主要用于确保目标变量在扫描前是干净的初始状态
			// ScanUpdate (1 << 1) - 更新模式
			//   表示扫描操作是否应该更新现有值
			//   当设置此模式时，扫描会尝试更新目标变量中已有的值
			//   主要用于批量更新操作
			// ScanOnConflictDoNothing (1 << 2) - 冲突不处理模式
			//   表示在遇到冲突时是否应该跳过
			//   当设置此模式时，如果遇到冲突（如主键冲突），扫描操作会跳过该记录
			//   主要用于批量插入时避免重复记录
			mode := gorm.ScanUpdate
			//if c, ok := db.Statement.Clauses["ON CONFLICT"]; ok {
			//	if onConflict, _ := c.Expression.(clause.OnConflict); onConflict.DoNothing {
			// // 若OnConflict标示的是不处理的话，则scan时也不处理
			//		mode |= gorm.ScanOnConflictDoNothing
			//	}
			//}

			rows, err := db.Statement.ConnPool.QueryContext(
				db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...,
			)
			if db.AddError(err) == nil {
				defer func() {
					db.AddError(rows.Close())
				}()
				gorm.Scan(rows, db, mode)
			}

			return
		}

		result, err := db.Statement.ConnPool.ExecContext(
			db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...,
		)
		if err != nil {
			db.AddError(err)
			return
		}

		db.RowsAffected, _ = result.RowsAffected()
		if db.RowsAffected == 0 {
			return
		}

		var (
			pkField     *schema.Field
			pkFieldName = "@id"
		)

		// 获取插入的 ID
		//insertID, err := result.LastInsertId()
		insertID := int64(0)
		//TODO需进一步确定ConnPool是否和上面的ConnPool是同一个会话
		row := db.Statement.ConnPool.QueryRowContext(db.Statement.Context, "VALUES IDENTITY_VAL_LOCAL()", []interface{}{}...)
		if err := row.Err(); err != nil {
			db.AddError(err)
			return
		}
		row.Scan(&insertID)

		insertOk := err == nil && insertID > 0

		if !insertOk {
			if !supportReturning {
				db.AddError(err)
			}
			return
		}

		if db.Statement.Schema != nil {
			if db.Statement.Schema.PrioritizedPrimaryField == nil || !db.Statement.Schema.PrioritizedPrimaryField.HasDefaultValue {
				return
			}
			pkField = db.Statement.Schema.PrioritizedPrimaryField
			pkFieldName = db.Statement.Schema.PrioritizedPrimaryField.DBName
		}

		// append @id column with value for auto-increment primary key
		// the @id value is correct, when: 1. without setting auto-increment primary key, 2. database AutoIncrementIncrement = 1
		switch values := db.Statement.Dest.(type) {
		case map[string]interface{}:
			values[pkFieldName] = insertID
		case *map[string]interface{}:
			(*values)[pkFieldName] = insertID
		case []map[string]interface{}, *[]map[string]interface{}:
			mapValues, ok := values.([]map[string]interface{})
			if !ok {
				if v, ok := values.(*[]map[string]interface{}); ok {
					if *v != nil {
						mapValues = *v
					}
				}
			}

			if config.LastInsertIDReversed {
				insertID -= int64(len(mapValues)-1) * schema.DefaultAutoIncrementIncrement
			}

			for _, mapValue := range mapValues {
				if mapValue != nil {
					mapValue[pkFieldName] = insertID
				}
				insertID += schema.DefaultAutoIncrementIncrement
			}
		default:
			if pkField == nil {
				return
			}

			switch db.Statement.ReflectValue.Kind() {
			case reflect.Slice, reflect.Array:
				if config.LastInsertIDReversed {
					for i := db.Statement.ReflectValue.Len() - 1; i >= 0; i-- {
						rv := db.Statement.ReflectValue.Index(i)
						if reflect.Indirect(rv).Kind() != reflect.Struct {
							break
						}

						_, isZero := pkField.ValueOf(db.Statement.Context, rv)
						if isZero {
							db.AddError(pkField.Set(db.Statement.Context, rv, insertID))
							insertID -= pkField.AutoIncrementIncrement
						}
					}
				} else {
					for i := 0; i < db.Statement.ReflectValue.Len(); i++ {
						rv := db.Statement.ReflectValue.Index(i)
						if reflect.Indirect(rv).Kind() != reflect.Struct {
							break
						}

						if _, isZero := pkField.ValueOf(db.Statement.Context, rv); isZero {
							db.AddError(pkField.Set(db.Statement.Context, rv, insertID))
							insertID += pkField.AutoIncrementIncrement
						}
					}
				}
			case reflect.Struct:
				_, isZero := pkField.ValueOf(db.Statement.Context, db.Statement.ReflectValue)
				if isZero {
					db.AddError(pkField.Set(db.Statement.Context, db.Statement.ReflectValue, insertID))
				}
			}
		}
	}
}
