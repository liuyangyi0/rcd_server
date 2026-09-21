"""Offline safety tests for the serial experiment runner; no serial I/O."""
import csv
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import serial_parity_matrix as matrix


class MatrixTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def fixture(self, name="one.csv", device="13", baud="28800"):
        with (self.root / name).open("w", newline="", encoding="utf-8-sig") as stream:
            csv.writer(stream).writerows([
                ["COM", "/dev/ttyWCH4"], ["BaudRate", baud], ["IsSpecial", "FALSE"],
                ["ComIndex", "1"], ["DeviceID", device], ["DataLen", "29"], ["Pre", "test_"],
                ["KKS", "Type"], ["READY", "bool"],
            ])

    def test_reads_configuration_without_mutating_it(self):
        self.fixture()
        before = matrix.config_digest(self.root)
        self.assertEqual(matrix.load_ports(self.root), {"/dev/ttyWCH4": [(13, 29, 28800)]})
        self.assertEqual(matrix.config_digest(self.root), before)

    def test_duplicate_addresses_are_rejected(self):
        self.fixture()
        self.fixture("duplicate.csv")
        with self.assertRaises(ValueError):
            matrix.load_ports(self.root)

    def test_inconsistent_baud_is_rejected(self):
        self.fixture()
        self.fixture("second.csv", "14", "9600")
        with self.assertRaises(ValueError):
            matrix.load_ports(self.root)

    def test_out_of_range_address_is_rejected(self):
        self.fixture(device="256")
        with self.assertRaises(ValueError):
            matrix.load_ports(self.root)

    def test_config_change_changes_fingerprint(self):
        self.fixture()
        before = matrix.config_digest(self.root)
        self.fixture(device="14")
        self.assertNotEqual(matrix.config_digest(self.root), before)

    def test_new_config_file_changes_fingerprint(self):
        self.fixture()
        before = matrix.config_digest(self.root)
        self.fixture("second.csv", "14")
        self.assertNotEqual(matrix.config_digest(self.root), before)

    def test_gateway_detection_fails_closed(self):
        for code in (0, 1, 2):
            with self.subTest(code=code), patch.object(
                matrix.subprocess, "run", return_value=subprocess.CompletedProcess([], code)
            ):
                if code == 2:
                    with self.assertRaises(RuntimeError):
                        matrix.gateway_running()
                else:
                    self.assertEqual(matrix.gateway_running(), code == 0)

    def test_each_timing_phase_changes_only_one_option(self):
        for name in ("noclear", "gap100", "gap220", "address2", "earlyread", "pre0", "timeout600"):
            mode, rx, overrides = matrix.PHASES[name]
            self.assertEqual((mode, rx), ("delay", "raw"))
            self.assertEqual(len(overrides), 1)


if __name__ == "__main__":
    unittest.main()
