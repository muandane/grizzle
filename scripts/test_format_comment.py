#!/usr/bin/env python3
import importlib.util
import json
import os
import tempfile
import unittest

helper_path = os.path.join(os.path.dirname(__file__), "format-comment.py")
spec = importlib.util.spec_from_file_location("format_comment", helper_path)
fc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fc)

extract_pr_number = fc.extract_pr_number
find_existing_comment_id = fc.find_existing_comment_id
format_comment_markdown = fc.format_comment_markdown

class TestFormatComment(unittest.TestCase):
    def test_extract_pr_number(self):
        with tempfile.NamedTemporaryFile("w+", delete=False) as f:
            json.dump({"pull_request": {"number": 123}}, f)
            f_path = f.name
        try:
            self.assertEqual(extract_pr_number(f_path), "123")
        finally:
            os.remove(f_path)

        with tempfile.NamedTemporaryFile("w+", delete=False) as f:
            json.dump({"number": 456}, f)
            f_path = f.name
        try:
            self.assertEqual(extract_pr_number(f_path), "456")
        finally:
            os.remove(f_path)

        self.assertEqual(extract_pr_number("/nonexistent/file.json"), "")

    def test_find_existing_comment_id(self):
        comments = [
            {"id": 10, "body": "some standard comment"},
            {"id": 20, "body": "<!-- grizzle-action-comment -->\n## 🐻 Grizzle Migration Plan Summary"},
            {"id": 30, "body": "another comment"}
        ]
        with tempfile.NamedTemporaryFile("w+", delete=False) as f:
            json.dump(comments, f)
            f_path = f.name
        try:
            self.assertEqual(find_existing_comment_id(f_path), "20")
        finally:
            os.remove(f_path)

        # Anchor absent
        comments_no_anchor = [{"id": 10, "body": "some other comment"}]
        with tempfile.NamedTemporaryFile("w+", delete=False) as f:
            json.dump(comments_no_anchor, f)
            f_path = f.name
        try:
            self.assertEqual(find_existing_comment_id(f_path), "")
        finally:
            os.remove(f_path)

    def test_format_empty_plan(self):
        plan_data = {"steps": [], "hazards": [], "hash": "abc123hash"}
        md = format_comment_markdown(plan_data, {}, "")
        self.assertIn("<!-- grizzle-action-comment -->", md)
        self.assertIn("## 🐻 Grizzle Migration Plan Summary", md)
        self.assertIn("| **0** | **0** | **0** |", md)
        self.assertIn("Database schema is already in sync.", md)
        self.assertIn("*Plan Hash: `abc123hash`*", md)

    def test_format_with_steps_and_hazards(self):
        plan_data = {
            "steps": [
                {"type": "CREATE_TABLE", "table": "users", "sql": "CREATE TABLE users (id int);"},
                {"type": "ALTER_COLUMN", "table": "users", "column": "email", "sql": "ALTER TABLE users ALTER COLUMN email SET NOT NULL;"},
                {"type": "DROP_INDEX", "table": "users", "sql": "DROP INDEX idx_old;"}
            ],
            "hazards": [
                {
                    "level": "WARNING",
                    "code": "TYPE_NARROW",
                    "table": "users",
                    "description": "Column type changed to a narrower type"
                }
            ],
            "hash": "def456hash"
        }
        md = format_comment_markdown(plan_data, {}, "")
        self.assertIn("| **1** | **1** | **1** |", md)
        self.assertIn("Planned SQL Statements (3 steps)", md)
        self.assertIn("-- [CREATE_TABLE] users", md)
        self.assertIn("-- [ALTER_COLUMN] users.email", md)
        self.assertIn("-- [DROP_INDEX] users", md)
        self.assertIn("### ⚠️ Migration Hazards", md)
        self.assertIn("`TYPE_NARROW`", md)
        self.assertIn("🟠 `WARNING`", md)
        self.assertIn("*Plan Hash: `def456hash`*", md)

    def test_format_with_simulation(self):
        plan_data = {
            "steps": [
                {"type": "CREATE_TABLE", "table": "users", "sql": "CREATE TABLE users (id int);"}
            ],
            "hash": "sim123hash"
        }
        sim_data_ok = {
            "total_duration": 1500000,
            "step_timings": [
                {"step": {"type": "CREATE_TABLE", "table": "users"}, "duration": 1500000}
            ]
        }
        md_ok = format_comment_markdown(plan_data, sim_data_ok, "")
        self.assertIn("### ⏱️ Pre-Flight Simulation (Lock Profiling)", md_ok)
        self.assertIn("All transactional steps held locks within the configured threshold", md_ok)
        self.assertIn("1.5ms", md_ok)

        sim_data_warn = {
            "total_duration": 650000000,
            "step_timings": [
                {"step": {"type": "ALTER_TABLE", "table": "users"}, "duration": 650000000, "exceeded_lock_max": True}
            ],
            "exceeded_warnings": [
                "step 1 (ALTER_TABLE users) exceeded max lock duration: took 650ms (threshold: 500ms)"
            ]
        }
        md_warn = format_comment_markdown(plan_data, sim_data_warn, "")
        self.assertIn("> [!WARNING]", md_warn)
        self.assertIn("Lock duration threshold exceeded (1 violation):", md_warn)
        self.assertIn("took 650ms (threshold: 500ms)", md_warn)
        self.assertIn("⚠️ EXCEEDED", md_warn)

if __name__ == "__main__":
    unittest.main()
