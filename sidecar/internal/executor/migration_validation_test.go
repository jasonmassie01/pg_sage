package executor

import "testing"

func TestValidateExecutorSQLAllowsOnlyRehearsedOnlineMigrationShapes(t *testing.T) {
	allowed := []string{
		`ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_nn" ` +
			`CHECK ("email" IS NOT NULL) NOT VALID`,
		`ALTER TABLE "public"."users" VALIDATE CONSTRAINT "users_email_nn"`,
		`ALTER TABLE "public"."users" ALTER COLUMN "email" SET NOT NULL`,
		`ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" ` +
			`UNIQUE USING INDEX "users_email_key_idx"`,
		`ALTER TABLE "public"."users" DROP CONSTRAINT "users_email_nn"`,
	}
	for _, sql := range allowed {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("rehearsed migration SQL rejected: %q: %v", sql, err)
		}
	}
}

func TestValidateExecutorSQLRejectsUnboundedMigrationShapes(t *testing.T) {
	rejected := []string{
		`ALTER TABLE public.users DROP COLUMN email`,
		`ALTER TABLE public.users ALTER COLUMN email TYPE bigint`,
		`ALTER TABLE public.users ADD CONSTRAINT users_fk ` +
			`FOREIGN KEY (account_id) REFERENCES accounts(id)`,
		`ALTER TABLE public.users DROP CONSTRAINT users_email_key CASCADE`,
	}
	for _, sql := range rejected {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("unsafe migration SQL accepted: %q", sql)
		}
	}
}
