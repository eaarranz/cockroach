// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scplan_test

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scbuild"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scdeps/sctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scgraph"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scgraphviz"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestPlanAlterTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	datadriven.Walk(t, filepath.Join("testdata"), func(t *testing.T, path string) {
		s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
		defer s.Stopper().Stop(ctx)

		tdb := sqlutils.MakeSQLRunner(sqlDB)
		run := func(t *testing.T, d *datadriven.TestData) string {
			switch d.Cmd {
			case "create-view", "create-sequence", "create-table", "create-type", "create-database", "create-schema":
				stmts, err := parser.Parse(d.Input)
				require.NoError(t, err)
				require.Len(t, stmts, 1)
				tableName := ""
				switch node := stmts[0].AST.(type) {
				case *tree.CreateTable:
					tableName = node.Table.String()
				case *tree.CreateSequence:
					tableName = node.Name.String()
				case *tree.CreateView:
					tableName = node.Name.String()
				case *tree.CreateType:
					tableName = ""
				case *tree.CreateDatabase:
					tableName = ""
				case *tree.CreateSchema:
					tableName = ""
				default:
					t.Fatal("not a CREATE TABLE/SEQUENCE/VIEW statement")
				}
				tdb.Exec(t, d.Input)

				if len(tableName) > 0 {
					var tableID descpb.ID
					tdb.QueryRow(t, `SELECT $1::regclass::int`, tableName).Scan(&tableID)
					if tableID == 0 {
						t.Fatalf("failed to read ID of new table %s", tableName)
					}
					t.Logf("created relation with id %d", tableID)
				}
				return ""
			case "ops", "deps":
				var plan scplan.Plan
				sctestutils.WithBuilderDependenciesFromTestServer(s, func(deps scbuild.Dependencies) {
					stmts, err := parser.Parse(d.Input)
					require.NoError(t, err)
					var outputNodes scpb.State
					for i := range stmts {
						outputNodes, err = scbuild.Build(ctx, deps, outputNodes, stmts[i].AST)
						require.NoError(t, err)
					}

					plan, err = scplan.MakePlan(outputNodes,
						scplan.Params{
							ExecutionPhase: scop.PostCommitPhase,
						})
					require.NoError(t, err)
				})

				if d.Cmd == "ops" {
					return marshalOps(t, &plan)
				}
				return marshalDeps(t, &plan)
			case "unimplemented":
				sctestutils.WithBuilderDependenciesFromTestServer(s, func(deps scbuild.Dependencies) {
					stmts, err := parser.Parse(d.Input)
					require.NoError(t, err)
					require.Len(t, stmts, 1)

					stmt := stmts[0]
					alter, ok := stmt.AST.(*tree.AlterTable)
					require.Truef(t, ok, "not an ALTER TABLE statement: %s", stmt.SQL)
					_, err = scbuild.Build(ctx, deps, nil, alter)
					require.Truef(t, scbuild.HasNotImplemented(err), "expected unimplemented, got %v", err)
				})
				return ""

			default:
				return fmt.Sprintf("unknown command: %s", d.Cmd)
			}
		}
		datadriven.RunTest(t, path, run)
	})
}

// indentText indents text for formatting out marshaled data.
func indentText(input string, tab string) string {
	result := strings.Builder{}
	scanner := bufio.NewScanner(strings.NewReader(input))
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		line := scanner.Text()
		result.WriteString(tab)
		result.WriteString(line)
		result.WriteString("\n")
	}
	return result.String()
}

// marshalDeps marshals dependencies in scplan.Plan to a string.
func marshalDeps(t *testing.T, plan *scplan.Plan) string {
	var sortedDeps []string
	err := plan.Graph.ForEachNode(func(n *scpb.Node) error {
		return plan.Graph.ForEachDepEdgeFrom(n, func(de *scgraph.DepEdge) error {
			var deps strings.Builder
			fmt.Fprintf(&deps, "- from: [%s, %s]\n",
				screl.ElementString(de.From().Element()), de.From().Status)
			fmt.Fprintf(&deps, "  to:   [%s, %s]\n",
				screl.ElementString(de.To().Element()), de.To().Status)
			sortedDeps = append(sortedDeps, deps.String())
			return nil
		})
	})
	if err != nil {
		panic(errors.Wrap(err, "failed marshaling dependencies."))
	}
	// Lexicographically sort the dependencies,
	// since the order is not fully deterministic.
	sort.Strings(sortedDeps)
	var stages strings.Builder
	for _, dep := range sortedDeps {
		fmt.Fprintf(&stages, "%s", dep)
	}
	return stages.String()
}

// marshalOps marshals operations in scplan.Plan to a string.
func marshalOps(t *testing.T, plan *scplan.Plan) string {
	var stages strings.Builder
	for stageIdx, stage := range plan.Stages {
		_, _ = fmt.Fprintf(&stages, "Stage %d", stageIdx)
		if !stage.Revertible {
			stages.WriteString(" (non-revertible)")
		}
		stages.WriteString("\n")
		stageOps := ""
		for _, op := range stage.Ops.Slice() {
			opMap, err := scgraphviz.ToMap(op)
			require.NoError(t, err)
			data, err := yaml.Marshal(opMap)
			require.NoError(t, err)
			stageOps += fmt.Sprintf("%T\n%s", op, indentText(string(data), "  "))
		}
		stages.WriteString(indentText(stageOps, "  "))
	}
	return stages.String()
}
