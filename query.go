package ibmdb

// query.go 实现GORM的查询构建和执行功能
// 主要包含Query和BuildQuerySQL两个核心函数

import (
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils"
)

// Query 执行查询操作
// 1. 调用BuildQuerySQL构建SQL语句
// 2. 如果非DryRun模式且无错误，执行查询并扫描结果
func Query(db *gorm.DB) {
	if db.Error == nil {
		BuildQuerySQL(db)

		if !db.DryRun && db.Error == nil {
			rows, err := db.Statement.ConnPool.QueryContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
			if err != nil {
				db.AddError(err)
				return
			}
			defer func() {
				db.AddError(rows.Close())
			}()
			gorm.Scan(rows, db, 0)
		}
	}
}

// BuildQuerySQL 构建查询SQL
// 1. 添加Schema的查询子句
// 2. 初始化SQL并构建SELECT子句
// 3. 处理结构体类型的主键条件
// 4. 处理Selects和Omits字段
// 5. 处理Join关系
// 6. 构建最终的SQL语句
func BuildQuerySQL(db *gorm.DB) {
	// 1. 添加Schema的查询子句
	if db.Statement.Schema != nil {
		for _, c := range db.Statement.Schema.QueryClauses {
			db.Statement.AddClause(c)
		}
	}

	// 2. 初始化SQL并构建SELECT子句
	if db.Statement.SQL.Len() == 0 {
		db.Statement.SQL.Grow(100) // 预分配100字节空间

		clauseSelect := clause.Select{Distinct: db.Statement.Distinct} // 处理DISTINCT

		// 3. 处理结构体类型的主键条件
		if db.Statement.ReflectValue.Kind() == reflect.Struct && db.Statement.ReflectValue.Type() == db.Statement.Schema.ModelType {
			var conds []clause.Expression
			for _, primaryField := range db.Statement.Schema.PrimaryFields {
				// 获取主键值，如果非零值则添加条件
				if v, isZero := primaryField.ValueOf(db.Statement.Context, db.Statement.ReflectValue); !isZero {
					conds = append(conds, clause.Eq{
						Column: clause.Column{Table: db.Statement.Table, Name: primaryField.DBName},
						Value:  v,
					})
				}
			}

			// 如果有条件则添加WHERE子句
			if len(conds) > 0 {
				db.Statement.AddClause(clause.Where{Exprs: conds})
			}
		}

		// 4. 处理Selects和Omits字段
		if len(db.Statement.Selects) > 0 {
			clauseSelect.Columns = make([]clause.Column, len(db.Statement.Selects))
			for idx, name := range db.Statement.Selects {
				if db.Statement.Schema == nil {
					clauseSelect.Columns[idx] = clause.Column{Name: name, Raw: true} // 原始SQL
				} else if f := db.Statement.Schema.LookUpField(name); f != nil {
					clauseSelect.Columns[idx] = clause.Column{Name: f.DBName} // 使用字段的DBName
				} else {
					clauseSelect.Columns[idx] = clause.Column{Name: name, Raw: true} // 未找到字段，使用原始SQL
				}
			}
		} else if db.Statement.Schema != nil && len(db.Statement.Omits) > 0 {
			// 处理Omits字段
			selectColumns, _ := db.Statement.SelectAndOmitColumns(false, false)
			clauseSelect.Columns = make([]clause.Column, 0, len(db.Statement.Schema.DBNames))
			for _, dbName := range db.Statement.Schema.DBNames {
				if v, ok := selectColumns[dbName]; (ok && v) || !ok {
					clauseSelect.Columns = append(clauseSelect.Columns, clause.Column{
						Table: db.Statement.Table,
						Name:  dbName,
					})
				}
			}
		} else if db.Statement.Schema != nil && db.Statement.ReflectValue.IsValid() {
			// 处理查询字段
			queryFields := db.QueryFields
			if !queryFields {
				switch db.Statement.ReflectValue.Kind() {
				case reflect.Struct:
					queryFields = db.Statement.ReflectValue.Type() != db.Statement.Schema.ModelType
				case reflect.Slice:
					queryFields = db.Statement.ReflectValue.Type().Elem() != db.Statement.Schema.ModelType
				}
			}

			if queryFields {
				stmt := gorm.Statement{DB: db}
				// 处理较小的结构体
				if err := stmt.Parse(db.Statement.Dest); err == nil && (db.QueryFields || stmt.Schema.ModelType != db.Statement.Schema.ModelType) {
					clauseSelect.Columns = make([]clause.Column, len(stmt.Schema.DBNames))

					for idx, dbName := range stmt.Schema.DBNames {
						clauseSelect.Columns[idx] = clause.Column{
							Table: db.Statement.Table,
							Name:  dbName,
						}
					}
				}
			}
		}

		// 5. 处理Join关系
		fromClause := clause.From{}
		if v, ok := db.Statement.Clauses["FROM"].Expression.(clause.From); ok {
			fromClause = v
		}

		// 如果有显式或隐式的Join
		if len(db.Statement.Joins) != 0 || len(fromClause.Joins) != 0 {
			if len(db.Statement.Selects) == 0 && len(db.Statement.Omits) == 0 && db.Statement.Schema != nil {
				clauseSelect.Columns = make([]clause.Column, len(db.Statement.Schema.DBNames))
				for idx, dbName := range db.Statement.Schema.DBNames {
					clauseSelect.Columns[idx] = clause.Column{Table: db.Statement.Table, Name: dbName}
				}
			}

			specifiedRelationsName := make(map[string]interface{})
			for _, join := range db.Statement.Joins {
				if db.Statement.Schema != nil {
					var isRelations bool // is relations or raw sql
					var relations []*schema.Relationship
					relation, ok := db.Statement.Schema.Relationships.Relations[join.Name]
					if ok {
						isRelations = true
						relations = append(relations, relation)
					} else {
						// handle nested join like "Manager.Company"
						nestedJoinNames := strings.Split(join.Name, ".")
						if len(nestedJoinNames) > 1 {
							isNestedJoin := true
							gussNestedRelations := make([]*schema.Relationship, 0, len(nestedJoinNames))
							currentRelations := db.Statement.Schema.Relationships.Relations
							for _, relname := range nestedJoinNames {
								// incomplete match, only treated as raw sql
								if relation, ok = currentRelations[relname]; ok {
									gussNestedRelations = append(gussNestedRelations, relation)
									currentRelations = relation.FieldSchema.Relationships.Relations
								} else {
									isNestedJoin = false
									break
								}
							}

							if isNestedJoin {
								isRelations = true
								relations = gussNestedRelations
							}
						}
					}

					if isRelations {
						genJoinClause := func(joinType clause.JoinType, parentTableName string, relation *schema.Relationship) clause.Join {
							tableAliasName := relation.Name
							if parentTableName != clause.CurrentTable {
								tableAliasName = utils.NestedRelationName(parentTableName, tableAliasName)
							}

							columnStmt := gorm.Statement{
								Table: tableAliasName, DB: db, Schema: relation.FieldSchema,
								Selects: join.Selects, Omits: join.Omits,
							}

							selectColumns, restricted := columnStmt.SelectAndOmitColumns(false, false)
							for _, s := range relation.FieldSchema.DBNames {
								if v, ok := selectColumns[s]; (ok && v) || (!ok && !restricted) {
									clauseSelect.Columns = append(clauseSelect.Columns, clause.Column{
										Table: tableAliasName,
										Name:  s,
										Alias: utils.NestedRelationName(tableAliasName, s),
									})
								}
							}

							exprs := make([]clause.Expression, len(relation.References))
							for idx, ref := range relation.References {
								if ref.OwnPrimaryKey {
									exprs[idx] = clause.Eq{
										Column: clause.Column{Table: parentTableName, Name: ref.PrimaryKey.DBName},
										Value:  clause.Column{Table: tableAliasName, Name: ref.ForeignKey.DBName},
									}
								} else {
									if ref.PrimaryValue == "" {
										exprs[idx] = clause.Eq{
											Column: clause.Column{Table: parentTableName, Name: ref.ForeignKey.DBName},
											Value:  clause.Column{Table: tableAliasName, Name: ref.PrimaryKey.DBName},
										}
									} else {
										exprs[idx] = clause.Eq{
											Column: clause.Column{Table: tableAliasName, Name: ref.ForeignKey.DBName},
											Value:  ref.PrimaryValue,
										}
									}
								}
							}

							{
								onStmt := gorm.Statement{Table: tableAliasName, DB: db, Clauses: map[string]clause.Clause{}}
								for _, c := range relation.FieldSchema.QueryClauses {
									onStmt.AddClause(c)
								}

								if join.On != nil {
									onStmt.AddClause(join.On)
								}

								if cs, ok := onStmt.Clauses["WHERE"]; ok {
									if where, ok := cs.Expression.(clause.Where); ok {
										where.Build(&onStmt)

										if onSQL := onStmt.SQL.String(); onSQL != "" {
											vars := onStmt.Vars
											for idx, v := range vars {
												bindvar := strings.Builder{}
												onStmt.Vars = vars[0 : idx+1]
												db.Dialector.BindVarTo(&bindvar, &onStmt, v)
												onSQL = strings.Replace(onSQL, bindvar.String(), "?", 1)
											}

											exprs = append(exprs, clause.Expr{SQL: onSQL, Vars: vars})
										}
									}
								}
							}

							return clause.Join{
								Type:  joinType,
								Table: clause.Table{Name: relation.FieldSchema.Table, Alias: tableAliasName},
								ON:    clause.Where{Exprs: exprs},
							}
						}

						parentTableName := clause.CurrentTable
						for _, rel := range relations {
							// joins table alias like "Manager, Company, Manager__Company"
							nestedAlias := utils.NestedRelationName(parentTableName, rel.Name)
							if _, ok := specifiedRelationsName[nestedAlias]; !ok {
								fromClause.Joins = append(fromClause.Joins, genJoinClause(join.JoinType, parentTableName, rel))
								specifiedRelationsName[nestedAlias] = nil
							}

							if parentTableName != clause.CurrentTable {
								parentTableName = utils.NestedRelationName(parentTableName, rel.Name)
							} else {
								parentTableName = rel.Name
							}
						}
					} else {
						fromClause.Joins = append(fromClause.Joins, clause.Join{
							Expression: clause.NamedExpr{SQL: join.Name, Vars: join.Conds},
						})
					}
				} else {
					fromClause.Joins = append(fromClause.Joins, clause.Join{
						Expression: clause.NamedExpr{SQL: join.Name, Vars: join.Conds},
					})
				}
			}

			db.Statement.AddClause(fromClause)
		} else {
			db.Statement.AddClauseIfNotExists(clause.From{})
		}

		// 6. 构建最终的SQL语句
		db.Statement.AddClauseIfNotExists(clauseSelect)
		db.Statement.Build(db.Statement.BuildClauses...)
	}
}
