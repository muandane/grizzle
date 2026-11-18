package exec

import (
	"slices"
	"strings"

	"github.com/muandane/grizzle/internal/schema"
)

// maskShadowExtensions removes shadow-installed extensions from a live
// schema snapshot. The shadow compile best-effort installs CREATE EXTENSION
// into the shadow namespace inside the rolled-back compile tx; pg_extension
// is database-wide, so without masking the diff would treat those ephemeral
// installs as already-satisfied live state.
func maskShadowExtensions(live *schema.Schema, shadowSchemas []string) {
	if live == nil || len(live.Extensions) == 0 || len(shadowSchemas) == 0 {
		return
	}
	shadowSet := make(map[string]bool, len(shadowSchemas))
	for _, s := range shadowSchemas {
		shadowSet[s] = true
	}
	for name, ext := range live.Extensions {
		if ext != nil && shadowSet[ext.Schema] {
			delete(live.Extensions, name)
		}
	}
}

// unmapShadowExprs strips ephemeral shadow-schema and target-schema qualifications from column
// default expressions, generated expressions, data types, and dependencies. Functions re-installed
// into the shadow schema (e.g. extension functions captured via WITH SCHEMA rewriting) leave
// `_grizzle_shadow_<id>.fn(...)` in pg_get_expr output; the shadow schema is unique per run and
// never exists in the target, so left in place they break rendered DDL and cause permanent drift.
// Types formatted by format_type during shadow inspection omit targetSchema from search_path,
// causing them to be qualified as `targetSchema.<type>` when they belong to targetSchema.
// Stripping lets the reference resolve through the apply-time search_path.
func unmapShadowExprs(s *schema.Schema, shadowSchemas []string, targetSchemas ...string) {
	if s == nil || (len(shadowSchemas) == 0 && len(targetSchemas) == 0) {
		return
	}
	allPrefixes := append(slices.Clone(shadowSchemas), targetSchemas...)
	for _, t := range s.Tables {
		for _, c := range t.Columns {
			for _, shadow := range shadowSchemas {
				if shadow == "" {
					continue
				}
				c.DefaultValue = strings.ReplaceAll(c.DefaultValue, shadow+".", "")
				c.DefaultValue = strings.ReplaceAll(c.DefaultValue, `"`+shadow+`".`, "")
				if c.Generated != nil {
					c.Generated.Expr = strings.ReplaceAll(c.Generated.Expr, shadow+".", "")
					c.Generated.Expr = strings.ReplaceAll(c.Generated.Expr, `"`+shadow+`".`, "")
				}
			}
			c.DataType = stripQualifiers(c.DataType, allPrefixes)
		}
	}
	for _, comp := range s.CompositeTypes {
		for _, attr := range comp.Attributes {
			attr.DataType = stripQualifiers(attr.DataType, allPrefixes)
		}
	}
	for _, dom := range s.Domains {
		dom.BaseType = stripQualifiers(dom.BaseType, allPrefixes)
	}
	for _, r := range s.RangeTypes {
		r.Subtype = stripQualifiers(r.Subtype, allPrefixes)
	}
	for _, seq := range s.Sequences {
		seq.DataType = stripQualifiers(seq.DataType, allPrefixes)
	}
	for _, v := range s.Views {
		for i, dep := range v.DependsOn {
			for _, shadow := range shadowSchemas {
				if shadow == "" {
					continue
				}
				v.DependsOn[i] = strings.ReplaceAll(dep, shadow+".", "")
				v.DependsOn[i] = strings.ReplaceAll(v.DependsOn[i], `"`+shadow+`".`, "")
			}
		}
	}
	for _, r := range s.Routines {
		for i, dep := range r.DependsOn {
			for _, shadow := range shadowSchemas {
				if shadow == "" {
					continue
				}
				r.DependsOn[i] = strings.ReplaceAll(dep, shadow+".", "")
				r.DependsOn[i] = strings.ReplaceAll(r.DependsOn[i], `"`+shadow+`".`, "")
			}
		}
	}
}

// stripQualifiers removes qualified schema prefixes from a type identifier and normalizes it.
func stripQualifiers(raw string, prefixes []string) string {
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		raw = strings.ReplaceAll(raw, p+".", "")
		raw = strings.ReplaceAll(raw, `"`+p+`".`, "")
	}
	return schema.NormalizeType(raw)
}
