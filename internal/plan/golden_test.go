package plan_test

import (
	"testing"

	"github.com/muandane/grizzle/internal/plan"
)

func TestPlanHash_GoldenCorpus(t *testing.T) {
	tests := []struct {
		name         string
		plan         *plan.Plan
		expectedHash string
	}{
		{
			name: "empty_plan",
			plan: &plan.Plan{
				TargetSchema: "public",
			},
			expectedHash: "dbe5395abdaa4733e0f9b1109f411abb2ef19d32c4cccfc9e5ea138f578a4c5d",
		},
		{
			name: "sqlite_rebuild_step",
			plan: &plan.Plan{
				TargetSchema: "main",
				Steps: []plan.Step{
					{
						Type:           plan.ChangeDropColumn,
						Table:          "users",
						SQL:            `CREATE TABLE "_grizzle_new_users" ("id" INTEGER PRIMARY KEY, "name" TEXT); INSERT INTO "_grizzle_new_users" ("id", "name") SELECT "id", "name" FROM "users"; DROP TABLE "users"; ALTER TABLE "_grizzle_new_users" RENAME TO "users";`,
						Destructive:    true,
						IsTableRebuild: true,
					},
				},
			},
			expectedHash: "0f2db2eb6a91dcef254435e8889a75d61f727fbb937312b6e44514013ca25cd5",
		},
		{
			name: "postgres_complex_ddl",
			plan: &plan.Plan{
				TargetSchema: "public",
				Steps: []plan.Step{
					{
						Type:        plan.ChangeCreateEnum,
						Table:       "user_role",
						SQL:         `CREATE TYPE "public"."user_role" AS ENUM ('admin', 'member');`,
						Destructive: false,
					},
					{
						Type:        plan.ChangeCreateTable,
						Table:       "accounts",
						SQL:         `CREATE TABLE "public"."accounts" ("id" bigserial PRIMARY KEY, "role" "public"."user_role" NOT NULL);`,
						Destructive: false,
					},
					{
						Type:           plan.ChangeValidateConstraint,
						Table:          "accounts",
						SQL:            `ALTER TABLE "public"."accounts" VALIDATE CONSTRAINT "chk_role";`,
						ValidatesCheck: true,
					},
				},
			},
			expectedHash: "68bc74b749383de91d104c2b9c183a7c212ef85e9e04ecde743ef0ae5e1f60f6",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.plan.Hash()
			if got != tc.expectedHash {
				t.Errorf("Plan.Hash() mismatch for %q\n got: %s\nwant: %s", tc.name, got, tc.expectedHash)
			}
		})
	}
}

func TestPlanHash_ColumnsOmitzeroStability(t *testing.T) {
	p1 := &plan.Plan{
		TargetSchema: "main",
		Steps: []plan.Step{
			{
				Type:           plan.ChangeDropColumn,
				Table:          "users",
				SQL:            `CREATE TABLE "_grizzle_new_users" ("id" INTEGER PRIMARY KEY, "name" TEXT);`,
				Destructive:    true,
				IsTableRebuild: true,
			},
		},
	}

	p2 := &plan.Plan{
		TargetSchema: "main",
		Steps: []plan.Step{
			{
				Type:           plan.ChangeDropColumn,
				Table:          "users",
				SQL:            `CREATE TABLE "_grizzle_new_users" ("id" INTEGER PRIMARY KEY, "name" TEXT);`,
				Destructive:    true,
				IsTableRebuild: true,
				Columns:        []string{},
			},
		},
	}

	if p1.Hash() != p2.Hash() {
		t.Fatalf("expected nil Columns and empty Columns to produce identical hash: %s != %s", p1.Hash(), p2.Hash())
	}

	p3 := &plan.Plan{
		TargetSchema: "main",
		Steps: []plan.Step{
			{
				Type:           plan.ChangeDropColumn,
				Table:          "users",
				SQL:            `CREATE TABLE "_grizzle_new_users" ("id" INTEGER PRIMARY KEY, "name" TEXT);`,
				Destructive:    true,
				IsTableRebuild: true,
				Columns:        []string{"id", "name"},
			},
		},
	}
	if p3.Hash() == p1.Hash() {
		t.Fatal("expected populated Columns to produce distinct hash")
	}
}
