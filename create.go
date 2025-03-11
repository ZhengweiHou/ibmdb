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

		// 如果有 Schema 定义
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

			db.Statement.Build(db.Statement.BuildClauses...)
		}

		// 检查是否为 DryRun 模式
		isDryRun := !db.DryRun && db.Error == nil
		if !isDryRun {
			return
		}

		//		ok, mode := hasReturning(db, supportReturning)
		//		if ok {
		//			if c, ok := db.Statement.Clauses["ON CONFLICT"]; ok {
		//				if onConflict, _ := c.Expression.(clause.OnConflict); onConflict.DoNothing {
		//					mode |= gorm.ScanOnConflictDoNothing
		//				}
		//			}
		//
		//			rows, err := db.Statement.ConnPool.QueryContext(
		//				db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...,
		//			)
		//			if db.AddError(err) == nil {
		//				defer func() {
		//					db.AddError(rows.Close())
		//				}()
		//				gorm.Scan(rows, db, mode)
		//			}
		//
		//			return
		//		}

		// 执行 SQL 语句
		// 手动获取连接执行 SQL 语句
		result, err := db.Statement.ConnPool.ExecContext(
			db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...,
		)
		if err != nil {
			db.AddError(err)
			return
		}

		// 获取受影响的行数
		db.RowsAffected, _ = result.RowsAffected()
		if db.RowsAffected == 0 {
			return
		}

		var (
			pkField     *schema.Field
			pkFieldName = "@id"
			// 需要返回主键值
			needLastId = false
		)

		if db.Statement.Schema != nil {
			if len(db.Statement.Schema.PrimaryFields) == 1 &&
				db.Statement.Schema.PrimaryFields != nil &&
				db.Statement.Schema.PrioritizedPrimaryField.DataType == schema.Uint &&
				db.Statement.Schema.PrioritizedPrimaryField.HasDefaultValue {
				needLastId = true
				pkField = db.Statement.Schema.PrioritizedPrimaryField
				pkFieldName = pkField.DBName
			}
		}

		if !needLastId {
			return
		}

		// TODO TEST
		// 睡眠随机时间
		//		time.Sleep(time.Duration(rand.Intn(5000)) * time.Millisecond)

		// 获取插入的 ID
		insertID := int64(0)
		//TODO需进一步确定ConnPool是否和上面的ConnPool是同一个会话
		row := db.Statement.ConnPool.QueryRowContext(db.Statement.Context, "VALUES IDENTITY_VAL_LOCAL()", []interface{}{}...)
		if err := row.Err(); err != nil {
			db.AddError(err)
			return
		}
		row.Scan(&insertID)
		//insertID, err := result.LastInsertId()
		insertOk := err == nil && insertID > 0

		if !insertOk {
			if !supportReturning {
				db.AddError(err)
			}
			return
		}

		// 如果有 Schema 定义，获取主键字段
		if db.Statement.Schema != nil {
			if db.Statement.Schema.PrioritizedPrimaryField == nil || !db.Statement.Schema.PrioritizedPrimaryField.HasDefaultValue {
				return
			}
			pkField = db.Statement.Schema.PrioritizedPrimaryField
			pkFieldName = db.Statement.Schema.PrioritizedPrimaryField.DBName
		}

		// 根据不同的目标类型设置主键值
		// append @id column with value for auto-increment primary key
		// the @id value is correct, when: 1. without setting auto-increment primary key, 2. database AutoIncrementIncrement = 1
		switch values := db.Statement.Dest.(type) {
		case map[string]interface{}:
			// 如果目标类型是 map[string]interface{}，直接设置主键值
			values[pkFieldName] = insertID
		case *map[string]interface{}:
			// 如果目标类型是 *map[string]interface{}，解引用并设置主键值
			(*values)[pkFieldName] = insertID
		case []map[string]interface{}, *[]map[string]interface{}:
			// 如果目标类型是 []map[string]interface{} 或 *[]map[string]interface{}，处理每个 map
			mapValues, ok := values.([]map[string]interface{})
			if !ok {
				if v, ok := values.(*[]map[string]interface{}); ok {
					if *v != nil {
						mapValues = *v
					}
				}
			}

			// 如果配置了 LastInsertIDReversed，调整 insertID
			if config.LastInsertIDReversed {
				insertID -= int64(len(mapValues)-1) * schema.DefaultAutoIncrementIncrement
			}

			// 为每个 map 设置主键值
			for _, mapValue := range mapValues {
				if mapValue != nil {
					mapValue[pkFieldName] = insertID
				}
				insertID += schema.DefaultAutoIncrementIncrement
			}
		default:
			// 如果目标类型不是上述类型，检查是否有主键字段
			if pkField == nil {
				return
			}

			// 根据目标类型的反射值设置主键值
			switch db.Statement.ReflectValue.Kind() {
			case reflect.Slice, reflect.Array:
				// 如果目标类型是 Slice 或 Array，处理每个元素
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
				// 如果目标类型是 Struct，直接设置主键值
				_, isZero := pkField.ValueOf(db.Statement.Context, db.Statement.ReflectValue)
				if isZero {
					db.AddError(pkField.Set(db.Statement.Context, db.Statement.ReflectValue, insertID))
				}
			}
		}
	}
}
