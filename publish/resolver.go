package publish

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"github.com/jinzhu/gorm"
)

type Resolver struct {
	Records      []interface{}
	Dependencies map[string]*Dependency
	DB           *Publish
}

type Dependency struct {
	Type          reflect.Type
	PrimaryValues [][]interface{}
}

func IncludeValue(value []interface{}, values [][]interface{}) bool {
	for _, v := range values {
		if fmt.Sprintf("%v", v) == fmt.Sprintf("%v", value) {
			return true
		}
	}
	return false
}

func (resolver *Resolver) AddDependency(dependency *Dependency) {
	name := dependency.Type.String()
	var newPrimaryKeys [][]interface{}

	// append primary keys to dependency
	if dep, ok := resolver.Dependencies[name]; ok {
		for _, primaryKey := range dependency.PrimaryValues {
			if !IncludeValue(primaryKey, dep.PrimaryValues) {
				newPrimaryKeys = append(newPrimaryKeys, primaryKey)
				dep.PrimaryValues = append(dep.PrimaryValues, primaryKey)
			}
		}
	} else {
		resolver.Dependencies[name] = dependency
		newPrimaryKeys = dependency.PrimaryValues
	}

	if len(newPrimaryKeys) > 0 {
		resolver.GetDependencies(dependency, newPrimaryKeys)
	}
}

func (resolver *Resolver) GetDependencies(dependency *Dependency, primaryKeys [][]interface{}) {
	value := reflect.New(dependency.Type)
	fromScope := resolver.DB.DB.NewScope(value.Interface())

	draftDB := resolver.DB.DraftDB().Unscoped()
	for _, field := range fromScope.Fields() {
		if relationship := field.Relationship; relationship != nil {
			if IsPublishableModel(field.Field.Interface()) {
				toType := modelType(field.Field.Interface())
				toScope := draftDB.NewScope(reflect.New(toType).Interface())
				draftTable := DraftTableName(toScope.TableName())
				var dependencyKeys [][]interface{}
				var rows *sql.Rows
				var err error

				if relationship.Kind == "belongs_to" || relationship.Kind == "has_many" {
					sql := fmt.Sprintf("%v IN (?) and publish_status = ?", relationship.ForeignDBName)
					rows, err = draftDB.Table(draftTable).Select(toScope.PrimaryKey()).Where(sql, primaryKeys, DIRTY).Rows()
				} else if relationship.Kind == "has_one" {
					fromTable := fromScope.TableName()
					fromPrimaryKey := fromScope.PrimaryKey()
					toTable := toScope.TableName()
					toPrimaryKey := toScope.PrimaryKey()

					sql := fmt.Sprintf("%v.%v IN (select %v.%v from %v where %v.%v IN (?)) and %v.publish_status = ?",
						toTable, toPrimaryKey, fromTable, relationship.ForeignDBName, fromTable, fromTable, fromPrimaryKey, toTable)

					rows, err = draftDB.Table(draftTable).Select(toTable+"."+toPrimaryKey).Where(sql, primaryKeys, DIRTY).Rows()
				} else if relationship.Kind == "many_to_many" {
					relationship.JoinTableHandler.Table(draftDB)
				}

				if rows != nil && err == nil {
					for rows.Next() {
						var primaryValues = make([]interface{}, len(toScope.PrimaryFields()))
						rows.Scan(primaryValues...)
						dependencyKeys = append(dependencyKeys, primaryValues)
					}

					dependency := Dependency{Type: toType, PrimaryValues: dependencyKeys}
					resolver.AddDependency(&dependency)
				}
			}
		}
	}
}

func (resolver *Resolver) GenerateDependencies() {
	for _, record := range resolver.Records {
		if IsPublishableModel(record) {
			scope := resolver.DB.DB.NewScope(record)
			var primaryValues []interface{}
			for _, field := range scope.PrimaryFields() {
				primaryValues = append(primaryValues, field.Field.Interface())
			}
			dependency := Dependency{Type: modelType(record), PrimaryValues: [][]interface{}{primaryValues}}
			resolver.AddDependency(&dependency)
		}
	}
}

func (resolver *Resolver) Publish() error {
	resolver.GenerateDependencies()
	tx := resolver.DB.DB.Begin()

	for _, dependency := range resolver.Dependencies {
		value := reflect.New(dependency.Type).Elem()
		productionScope := resolver.DB.ProductionDB().NewScope(value.Addr().Interface())
		productionTable := productionScope.TableName()
		primaryKey := scopePrimaryKeys(productionScope)
		draftTable := DraftTableName(productionTable)

		var columns []string
		for _, field := range productionScope.Fields() {
			if field.IsNormal {
				columns = append(columns, field.DBName)
			}
		}

		var productionColumns []string
		for _, column := range columns {
			productionColumns = append(productionColumns, fmt.Sprintf("%v.%v", productionTable, column))
		}

		var draftColumns []string
		for _, column := range columns {
			draftColumns = append(draftColumns, fmt.Sprintf("%v.%v", draftTable, column))
		}

		if len(dependency.PrimaryValues) > 0 {
			deleteSql := fmt.Sprintf("DELETE FROM %v WHERE %v.%v IN (%v)", productionTable, productionTable, primaryKey, toQueryMarks(dependency.PrimaryValues))
			tx.Exec(deleteSql, toQueryValues(dependency.PrimaryValues)...)

			publishSql := fmt.Sprintf("INSERT INTO %v (%v) SELECT %v FROM %v WHERE %v.%v IN (%v)",
				productionTable, strings.Join(productionColumns, " ,"), strings.Join(draftColumns, " ,"),
				draftTable, draftTable, primaryKey, toQueryMarks(dependency.PrimaryValues))
			tx.Exec(publishSql, toQueryValues(dependency.PrimaryValues)...)

			updateStateSql := fmt.Sprintf("UPDATE %v SET publish_status = ? WHERE %v.%v IN (%v)", draftTable, draftTable, primaryKey, toQueryMarks(dependency.PrimaryValues))

			var params = []interface{}{bool(PUBLISHED)}
			params = append(params, toQueryValues(dependency.PrimaryValues)...)
			tx.Exec(updateStateSql, params...)
		}
	}

	if err := tx.Error; err == nil {
		return tx.Commit().Error
	} else {
		tx.Rollback()
		return err
	}
}

func (resolver *Resolver) Discard() error {
	resolver.GenerateDependencies()
	tx := resolver.DB.DB.Begin()

	for _, dependency := range resolver.Dependencies {
		value := reflect.New(dependency.Type).Elem()
		productionScope := resolver.DB.ProductionDB().NewScope(value.Addr().Interface())
		productionTable := productionScope.TableName()
		draftTable := DraftTableName(productionTable)

		primaryKey := scopePrimaryKeys(productionScope)

		var columns []string
		for _, field := range productionScope.Fields() {
			if field.IsNormal {
				columns = append(columns, field.DBName)
			}
		}

		var productionColumns []string
		for _, column := range columns {
			productionColumns = append(productionColumns, fmt.Sprintf("%v.%v", productionTable, column))
		}

		var draftColumns []string
		for _, column := range columns {
			draftColumns = append(draftColumns, fmt.Sprintf("%v.%v", draftTable, column))
		}

		deleteSql := fmt.Sprintf("DELETE FROM %v WHERE %v IN (%v)", draftTable, primaryKey, toQueryMarks(dependency.PrimaryValues))
		tx.Exec(deleteSql, toQueryValues(dependency.PrimaryValues)...)

		discardSql := fmt.Sprintf("INSERT INTO %v (%v) SELECT %v FROM %v WHERE %v IN (%v)",
			draftTable, strings.Join(draftColumns, " ,"),
			strings.Join(productionColumns, " ,"), productionTable,
			primaryKey, toQueryMarks(dependency.PrimaryValues))
		tx.Exec(discardSql, toQueryValues(dependency.PrimaryValues)...)
	}

	if err := tx.Error; err == nil {
		return tx.Commit().Error
	} else {
		tx.Rollback()
		return err
	}
}

func scopePrimaryKeys(scope *gorm.Scope) string {
	var primaryKeys []string
	for _, field := range scope.PrimaryFields() {
		key := fmt.Sprintf("%v", scope.Quote(field.DBName))
		primaryKeys = append(primaryKeys, key)
	}
	if len(primaryKeys) > 1 {
		return fmt.Sprintf("(%v)", strings.Join(primaryKeys, ","))
	}
	return strings.Join(primaryKeys, "")
}

func toQueryMarks(primaryValues [][]interface{}) string {
	var results []string

	for _, primaryValue := range primaryValues {
		var marks []string
		for range primaryValue {
			marks = append(marks, "?")
		}

		if len(marks) > 1 {
			results = append(results, fmt.Sprintf("(%v)", strings.Join(marks, ",")))
		} else {
			results = append(results, strings.Join(marks, ""))
		}
	}
	return strings.Join(results, ",")
}

func toQueryValues(primaryValues [][]interface{}) (values []interface{}) {
	for _, primaryValue := range primaryValues {
		for _, value := range primaryValue {
			values = append(values, value)
		}
	}
	return values
}
