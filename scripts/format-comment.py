#!/usr/bin/env python3
"""
format-comment.py: Formats sticky pull request migration summaries for Grizzle GitHub Action.
Generates GitHub-flavored markdown with diff metric badges, collapsible SQL statements,
hazard warnings, and simulation lock duration profiling.
"""

import argparse
import json
import os
import re
import sys

def parse_args():
    parser = argparse.ArgumentParser(description="Grizzle Action PR Comment Helper")
    parser.add_argument("--format", action="store_true", help="Generate comment markdown and API payload")
    parser.add_argument("--plan", default="", help="Path to plan JSON file")
    parser.add_argument("--sim", default="", help="Path to simulation JSON file")
    parser.add_argument("--output", default="", help="Path to raw CLI output text")
    parser.add_argument("--out-md", default="", help="Destination path for generated markdown")
    parser.add_argument("--out-payload", default="", help="Destination path for GitHub API JSON payload")
    parser.add_argument("--find-comment", default="", help="Path to GitHub issue comments JSON to search for anchor")
    parser.add_argument("--pr-number", default="", help="Path to GitHub event JSON to extract PR number")
    return parser.parse_args()

def extract_pr_number(event_path):
    if not event_path or not os.path.exists(event_path):
        return ""
    try:
        with open(event_path, "r", encoding="utf-8") as f:
            data = json.load(f)
        pr_num = data.get("pull_request", {}).get("number") or data.get("number")
        return str(pr_num) if pr_num else ""
    except Exception:
        return ""

def find_existing_comment_id(comments_path):
    if not comments_path or not os.path.exists(comments_path):
        return ""
    try:
        with open(comments_path, "r", encoding="utf-8") as f:
            comments = json.load(f)
        if isinstance(comments, list):
            for c in comments:
                body = c.get("body", "")
                if "<!-- grizzle-action-comment -->" in body:
                    cid = c.get("id")
                    return str(cid) if cid else ""
    except Exception:
        return ""
    return ""

def format_comment_markdown(plan_data, sim_data, output_text):
    lines = []
    lines.append("<!-- grizzle-action-comment -->")
    lines.append("## 🐻 Grizzle Migration Plan Summary")
    lines.append("")

    steps = plan_data.get("steps") or plan_data.get("plan", {}).get("steps") or []
    hazards = plan_data.get("hazards") or []
    plan_hash = plan_data.get("hash") or ""
    if not plan_hash and "Plan Hash:" in output_text:
        m = re.search(r"Plan Hash:\s*([a-f0-9]{64})", output_text)
        if m:
            plan_hash = m.group(1)

    add_prefixes = ("CREATE_", "ADD_", "ATTACH_", "ENABLE_", "GRANT")
    drop_prefixes = ("DROP_", "DETACH_")

    adds = 0
    alters = 0
    drops = 0

    for s in steps:
        st = s.get("type", "")
        if any(st.startswith(p) for p in add_prefixes):
            adds += 1
        elif any(st.startswith(p) for p in drop_prefixes):
            drops += 1
        else:
            alters += 1

    if len(steps) == 0 and "Plan:" in output_text:
        m = re.search(r"Plan:\s*(\d+)\s+to add,\s*(\d+)\s+to alter,\s*(\d+)\s+to destroy", output_text)
        if m:
            adds = int(m.group(1))
            alters = int(m.group(2))
            drops = int(m.group(3))

    # Metric Table
    lines.append("| 🟢 Adds | 🟡 Alters | 🔴 Drops |")
    lines.append("| :---: | :---: | :---: |")
    lines.append(f"| **{adds}** | **{alters}** | **{drops}** |")
    lines.append("")

    # Planned SQL Statements details
    if len(steps) == 0 and adds == 0 and alters == 0 and drops == 0:
        lines.append("> ℹ️ Database schema is already in sync. No migration steps planned.")
        lines.append("")
    elif len(steps) > 0:
        lines.append("<details>")
        lines.append(f"<summary><b>Planned SQL Statements ({len(steps)} step{'s' if len(steps) != 1 else ''})</b></summary>")
        lines.append("")
        lines.append("```sql")
        for i, s in enumerate(steps):
            st = s.get("type", "")
            target = s.get("table", "")
            if s.get("column"):
                target = f"{target}.{s.get('column')}"
            sql = s.get("sql", "").strip()
            if target:
                lines.append(f"-- [{st}] {target}")
            else:
                lines.append(f"-- [{st}]")
            lines.append(sql)
            if i < len(steps) - 1:
                lines.append("")
        lines.append("```")
        lines.append("</details>")
        lines.append("")

    # Migration Hazards section
    if len(hazards) > 0:
        lines.append("### ⚠️ Migration Hazards")
        lines.append("")
        lines.append("| Level | Code | Target | Description |")
        lines.append("| :--- | :--- | :--- | :--- |")
        for h in hazards:
            level = h.get("level", "NOTICE")
            code = h.get("code", "")
            target = h.get("table", "-")
            desc = h.get("description", "")
            icon = "🟡"
            if level == "CRITICAL":
                icon = "🔴"
            elif level == "WARNING":
                icon = "🟠"
            lines.append(f"| {icon} `{level}` | `{code}` | `{target}` | {desc} |")
        lines.append("")
    elif "Detected Hazards:" in output_text:
        lines.append("### ⚠️ Migration Hazards")
        lines.append("")
        lines.append("```")
        h_idx = output_text.find("Detected Hazards:")
        h_text = output_text[h_idx:]
        if "Plan Hash:" in h_text:
            h_text = h_text[:h_text.find("Plan Hash:")].strip()
        lines.append(h_text)
        lines.append("```")
        lines.append("")

    # Simulation lock duration summary
    has_sim = False
    sim_warnings = []
    sim_timings = []
    sim_total = None

    if sim_data:
        has_sim = True
        sim_warnings = sim_data.get("exceeded_warnings") or []
        sim_timings = sim_data.get("step_timings") or []
        dur_ns = sim_data.get("total_duration")
        if dur_ns is not None:
            if dur_ns < 1000:
                sim_total = f"{dur_ns}ns"
            elif dur_ns < 1_000_000:
                sim_total = f"{dur_ns / 1_000:.1f}µs"
            elif dur_ns < 1_000_000_000:
                sim_total = f"{dur_ns / 1_000_000:.1f}ms"
            else:
                sim_total = f"{dur_ns / 1_000_000_000:.2f}s"
    elif "Simulated Migration Plan" in output_text or "STEP   TYPE" in output_text:
        has_sim = True
        warn_match = re.search(r"Warnings \(\d+ threshold violations[^\)]*\):\n((?:  ! .*\n?)+)", output_text)
        if warn_match:
            for w in warn_match.group(1).strip().splitlines():
                sim_warnings.append(w.lstrip(" !").strip())

    if has_sim:
        lines.append("### ⏱️ Pre-Flight Simulation (Lock Profiling)")
        lines.append("")
        if sim_warnings:
            lines.append("> [!WARNING]")
            lines.append(f"> **Lock duration threshold exceeded ({len(sim_warnings)} violation{'s' if len(sim_warnings) != 1 else ''}):**")
            for w in sim_warnings:
                lines.append(f"> - {w}")
            lines.append("")
        else:
            time_str = f" (total time: {sim_total})" if sim_total else ""
            lines.append(f"> ✅ All transactional steps held locks within the configured threshold{time_str}.")
            lines.append("")

        if sim_timings:
            lines.append("<details>")
            lines.append("<summary><b>Step Lock Timings</b></summary>")
            lines.append("")
            lines.append("| Step | Type | Target | Duration | Lock Status |")
            lines.append("| :---: | :--- | :--- | :--- | :--- |")
            for i, st in enumerate(sim_timings):
                step_obj = st.get("step", {})
                st_type = step_obj.get("type", "-")
                st_target = step_obj.get("table", "-")
                if step_obj.get("column"):
                    st_target = f"{st_target}.{step_obj.get('column')}"
                dur_val = st.get("duration", 0)
                if dur_val < 1000:
                    d_str = f"{dur_val}ns"
                elif dur_val < 1_000_000:
                    d_str = f"{dur_val / 1000:.1f}µs"
                elif dur_val < 1_000_000_000:
                    d_str = f"{dur_val / 1_000_000:.1f}ms"
                else:
                    d_str = f"{dur_val / 1_000_000_000:.2f}s"
                status = "OK"
                if step_obj.get("non_tx"):
                    status = "SKIPPED"
                elif st.get("exceeded_lock_max"):
                    status = "⚠️ EXCEEDED"
                lines.append(f"| {i+1} | `{st_type}` | `{st_target}` | {d_str} | {status} |")
            lines.append("</details>")
            lines.append("")

    if plan_hash:
        lines.append(f"*Plan Hash: `{plan_hash}`*")

    return "\n".join(lines)

def main():
    args = parse_args()

    if args.pr_number:
        sys.stdout.write(extract_pr_number(args.pr_number))
        return

    if args.find_comment:
        sys.stdout.write(find_existing_comment_id(args.find_comment))
        return

    if args.format:
        plan_data = {}
        if args.plan and os.path.exists(args.plan):
            try:
                with open(args.plan, "r", encoding="utf-8") as f:
                    plan_data = json.load(f)
            except Exception as e:
                print(f"Warning reading plan JSON: {e}", file=sys.stderr)

        sim_data = {}
        if args.sim and os.path.exists(args.sim):
            try:
                with open(args.sim, "r", encoding="utf-8") as f:
                    sim_data = json.load(f)
            except Exception as e:
                print(f"Warning reading sim JSON: {e}", file=sys.stderr)

        output_text = ""
        if args.output and os.path.exists(args.output):
            try:
                with open(args.output, "r", encoding="utf-8", errors="replace") as f:
                    output_text = f.read()
            except Exception as e:
                print(f"Warning reading output text: {e}", file=sys.stderr)

        md = format_comment_markdown(plan_data, sim_data, output_text)

        if args.out_md:
            with open(args.out_md, "w", encoding="utf-8") as f:
                f.write(md)

        if args.out_payload:
            with open(args.out_payload, "w", encoding="utf-8") as f:
                json.dump({"body": md}, f)

        if not args.out_md and not args.out_payload:
            sys.stdout.write(md)

if __name__ == "__main__":
    main()
