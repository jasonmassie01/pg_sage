package catalogread

import "fmt"

// HeapExpectedPagesSQL is a SQL expression for the heap pages the analyzed
// rows of relation alias class (namespace alias namespace) need: reltuples
// times the average row footprint (the 23-byte tuple header and the
// pg_stats column widths, weighted by their non-null fraction and
// MAXALIGNed, plus the 4-byte line pointer) over the usable page bytes at
// the table's fillfactor. It reads pg_class, pg_stats and the reloptions
// only: no relation is opened or stat()ed. It is NULL when the relation was
// never analyzed (reltuples < 0) or no column statistics are visible, so a
// caller has no estimate rather than a guess.
func HeapExpectedPagesSQL(class, namespace string) string {
	return fmt.Sprintf(`(SELECT CASE WHEN %[1]s.reltuples >= 0 THEN ceil(
    %[1]s.reltuples * (ceil((23 + sum((1 - st.null_frac) * st.avg_width)::float8) / 8) * 8 + 4)
    / ((current_setting('block_size')::float8 - 24)
       * COALESCE((SELECT ro.option_value::float8
                     FROM pg_options_to_table(%[1]s.reloptions) ro
                    WHERE ro.option_name = 'fillfactor'), 100) / 100)) END
   FROM pg_stats st
  WHERE st.schemaname = %[2]s.nspname AND st.tablename = %[1]s.relname
    AND NOT st.inherited)`, class, namespace)
}
