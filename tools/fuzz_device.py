#!/usr/bin/env python3
"""
下位机健壮性模糊测试工具 (Device Robustness Fuzz Tester)

通过串口向下位机发送各种畸形、异常、随机数据，测试设备是否会宕机或停止响应。
协议帧格式: [DeviceID][CMD][SubCMD][Page][Offset][DataLen][CRC]
CRC = sum(前面所有字节) & 0xFF
"""

from __future__ import annotations

import os
import random
import struct
import threading
import time
import queue
from datetime import datetime
from typing import Optional

import serial
import serial.tools.list_ports
import tkinter as tk
from tkinter import ttk, scrolledtext, filedialog, messagebox

# ============================================================
#  协议常量 (从 Rust 源码 src/protocol.rs 提取)
# ============================================================

READ_STATUS = 0x08
READ_SUBCMD = 0x02
STATUS_PAGE = 0x30
STATUS_OFFSET = 0x10

DEFAULT_BAUD = 28800
DEFAULT_TIMEOUT = 0.4  # 400ms


def compute_crc(data: bytes) -> int:
    return sum(data) & 0xFF


def build_poll_frame(device_id: int, data_len: int) -> bytes:
    frame = bytes([device_id, READ_STATUS, READ_SUBCMD, STATUS_PAGE, STATUS_OFFSET, data_len])
    return frame + bytes([compute_crc(frame)])


def is_valid_status_response(data: Optional[bytes], device_id: int, data_len: int) -> bool:
    """验证设备状态响应的地址、命令、长度和校验和。"""
    if not data or len(data) < 5:
        return False

    declared_status_len = data[2] + 1
    expected_frame_len = data[2] + 5
    return (
        data[0] == device_id
        and data[1] == READ_STATUS
        and declared_status_len == data_len
        and len(data) == expected_frame_len
        and compute_crc(data[:-1]) == data[-1]
    )


def format_hex(data: bytes) -> str:
    return ' '.join(f'{b:02X}' for b in data)


# ============================================================
#  测试引擎
# ============================================================

class TestResult:
    __slots__ = ('category', 'test_name', 'sent', 'response', 'device_alive', 'duration_ms')

    def __init__(self, category: str, test_name: str, sent: bytes,
                 response: Optional[bytes], device_alive: Optional[bool], duration_ms: float):
        self.category = category
        self.test_name = test_name
        self.sent = sent
        self.response = response
        self.device_alive = device_alive
        self.duration_ms = duration_ms


class CategorySummary:
    __slots__ = ('name', 'display_name', 'total', 'responded', 'alive_after')

    def __init__(self, name: str, display_name: str):
        self.name = name
        self.display_name = display_name
        self.total = 0
        self.responded = 0
        self.alive_after: Optional[bool] = None

    def record_alive(self, alive: bool):
        if self.alive_after is None:
            self.alive_after = alive
        else:
            self.alive_after = self.alive_after and alive


# 测试类别定义
TEST_CATEGORIES = [
    ('valid',      '基线测试'),
    ('crc',        'CRC损坏'),
    ('length',     '长度异常'),
    ('truncated',  '截断帧'),
    ('oversized',  '超长帧'),
    ('commands',   '无效命令'),
    ('addresses',  '无效地址'),
    ('zeros_ones', '全0/全1'),
    ('random',     '随机数据'),
    ('rapid_fire', '极速连发'),
    ('timing',     '时序攻击'),
    ('repeated',   '重复字节'),
    ('boundary',   '帧边界混淆'),
    ('recovery',   '恢复验证'),
]


class FuzzRunner:
    def __init__(self, port_name: str, baud: int, device_id: int, data_len: int,
                 delay_ms: int, rounds: int, log_queue: queue.Queue,
                 progress_callback=None):
        self.port_name = port_name
        self.baud = baud
        self.device_id = device_id
        self.data_len = data_len
        self.delay_ms = delay_ms
        self.rounds = rounds
        self.log_queue = log_queue
        self.progress_callback = progress_callback
        self.port: Optional[serial.Serial] = None
        self.results: list[TestResult] = []
        self.summaries: dict[str, CategorySummary] = {}
        self._stop_event = threading.Event()
        self._total_steps = 0
        self._current_step = 0

    def stop(self):
        self._stop_event.set()

    @property
    def stopped(self) -> bool:
        return self._stop_event.is_set()

    def log(self, msg: str, level: str = 'INFO'):
        ts = datetime.now().strftime('%H:%M:%S.%f')[:-3]
        self.log_queue.put((level, f'[{ts}] {msg}'))

    def open_port(self) -> bool:
        try:
            self.port = serial.Serial(
                port=self.port_name,
                baudrate=self.baud,
                bytesize=serial.EIGHTBITS,
                stopbits=serial.STOPBITS_ONE,
                parity=serial.PARITY_NONE,
                timeout=DEFAULT_TIMEOUT,
                write_timeout=1.0,
            )
            self.log(f'串口 {self.port_name} 已打开 (波特率: {self.baud})')
            return True
        except serial.SerialException as e:
            self.log(f'串口打开失败: {e}', 'ERROR')
            return False

    def close_port(self):
        if self.port and self.port.is_open:
            self.port.close()
            self.log('串口已关闭')

    def flush_buffers(self):
        if self.port and self.port.is_open:
            self.port.reset_input_buffer()
            self.port.reset_output_buffer()

    def send_raw(self, data: bytes, read_timeout: float = None) -> Optional[bytes]:
        if not self.port or not self.port.is_open:
            return None
        old_timeout = self.port.timeout
        try:
            self.flush_buffers()
            self.port.write(data)
            self.port.flush()
            if read_timeout is not None:
                self.port.timeout = read_timeout
            resp = self.port.read(1024)
            return resp if resp else None
        except serial.SerialException as e:
            self.log(f'串口通信错误: {e}', 'ERROR')
            return None
        finally:
            if read_timeout is not None:
                self.port.timeout = old_timeout

    def send_bytes_slowly(self, data: bytes, byte_delay_ms: int) -> Optional[bytes]:
        if not self.port or not self.port.is_open:
            return None
        try:
            self.flush_buffers()
            for b in data:
                self.port.write(bytes([b]))
                self.port.flush()
                time.sleep(byte_delay_ms / 1000.0)
            resp = self.port.read(1024)
            return resp if resp else None
        except serial.SerialException as e:
            self.log(f'串口通信错误: {e}', 'ERROR')
            return None

    def check_alive(self) -> bool:
        frame = build_poll_frame(self.device_id, self.data_len)
        resp = self.send_raw(frame)
        return is_valid_status_response(resp, self.device_id, self.data_len)

    def valid_frame(self) -> bytes:
        return build_poll_frame(self.device_id, self.data_len)

    def run_test(self, category: str, name: str, data: bytes,
                 slow_delay_ms: int = 0, skip_alive_check: bool = False):
        if self.stopped:
            return

        self.log(f'[{category}/{name}] TX: {format_hex(data)}')

        t0 = time.monotonic()
        if slow_delay_ms > 0:
            resp = self.send_bytes_slowly(data, slow_delay_ms)
        else:
            resp = self.send_raw(data)
        elapsed = (time.monotonic() - t0) * 1000

        if resp:
            self.log(f'[{category}/{name}] RX: {format_hex(resp)}')
        else:
            self.log(f'[{category}/{name}] RX: <超时>')

        alive: Optional[bool] = None
        if not skip_alive_check:
            time.sleep(0.05)
            alive = self.check_alive()
            status = '存活' if alive else '宕机!!'
            level = 'INFO' if alive else 'ERROR'
            self.log(f'[{category}/{name}] 设备: {status}', level)

        result = TestResult(category, name, data, resp, alive, elapsed)
        self.results.append(result)

        if category in self.summaries:
            s = self.summaries[category]
            s.total += 1
            if resp:
                s.responded += 1
            if alive is not None:
                s.record_alive(alive)

        self._current_step += 1
        if self.progress_callback:
            self.progress_callback(self._current_step, self._total_steps)

        if self.delay_ms > 0:
            time.sleep(self.delay_ms / 1000.0)

    def _estimate_total_steps(self, categories: list[str]) -> int:
        total = 0
        for cat in categories:
            if cat == 'valid':
                total += 5
            elif cat == 'crc':
                total += 6
            elif cat == 'length':
                total += 8
            elif cat == 'truncated':
                total += 5
            elif cat == 'oversized':
                total += 4
            elif cat == 'commands':
                total += 256
            elif cat == 'addresses':
                total += 25
            elif cat == 'zeros_ones':
                total += 10
            elif cat == 'random':
                total += self.rounds * 12
            elif cat == 'rapid_fire':
                total += 2  # blast + alive check
            elif cat == 'timing':
                total += 8
            elif cat == 'repeated':
                total += 12
            elif cat == 'boundary':
                total += 6
            elif cat == 'recovery':
                total += 10
        return total

    def run_categories(self, categories: list[str]):
        self._stop_event.clear()
        self.results.clear()
        self.summaries.clear()
        self._current_step = 0
        self._total_steps = self._estimate_total_steps(categories)

        for cat_id, cat_name in TEST_CATEGORIES:
            if cat_id in categories:
                self.summaries[cat_id] = CategorySummary(cat_id, cat_name)

        if not self.open_port():
            self.log_queue.put(('DONE', False))
            return

        completed = False
        try:
            for cat_id in categories:
                if self.stopped:
                    break
                func = getattr(self, f'test_{cat_id}', None)
                if func:
                    self.log(f'===== 开始测试: {dict(TEST_CATEGORIES)[cat_id]} =====', 'HEADER')
                    self.flush_buffers()
                    func()
                    self.log(f'===== 完成: {dict(TEST_CATEGORIES)[cat_id]} =====', 'HEADER')
            completed = not self.stopped
        finally:
            try:
                self.close_port()
            finally:
                self.log_queue.put(('DONE', completed))

    # ---- 1. 基线测试 ----
    def test_valid(self):
        for i in range(5):
            if self.stopped:
                return
            self.run_test('valid', f'poll_{i+1}', self.valid_frame())

    # ---- 2. CRC 损坏 ----
    def test_crc(self):
        frame = list(self.valid_frame())
        correct_crc = frame[-1]

        tests = [
            ('off_by_plus1', (correct_crc + 1) & 0xFF),
            ('off_by_minus1', (correct_crc - 1) & 0xFF),
            ('crc_zero', 0x00),
            ('crc_0xFF', 0xFF),
            ('crc_inverted', (~correct_crc) & 0xFF),
            ('crc_random', random.randint(0, 255)),
        ]
        for name, crc_val in tests:
            if self.stopped:
                return
            bad_frame = bytes(frame[:-1] + [crc_val])
            self.run_test('crc', name, bad_frame)

    # ---- 3. 长度字段异常 ----
    def test_length(self):
        base = list(self.valid_frame())
        original_len = base[5]

        test_lens = [
            ('len_zero', 0x00),
            ('len_one', 0x01),
            ('len_0xFF', 0xFF),
            ('len_0xFE', 0xFE),
            ('len_plus1', (original_len + 1) & 0xFF),
            ('len_minus1', max(0, original_len - 1)),
            ('len_plus10', min(255, original_len + 10)),
            ('len_half', original_len // 2),
        ]
        for name, new_len in test_lens:
            if self.stopped:
                return
            bad = base.copy()
            bad[5] = new_len
            bad[-1] = compute_crc(bytes(bad[:-1]))  # recalc CRC with bad length
            self.run_test('length', name, bytes(bad))

    # ---- 4. 截断帧 ----
    def test_truncated(self):
        frame = self.valid_frame()
        tests = [
            ('1_byte', frame[:1]),
            ('2_bytes', frame[:2]),
            ('3_bytes', frame[:3]),
            ('half', frame[:len(frame)//2]),
            ('no_crc', frame[:-1]),
        ]
        for name, data in tests:
            if self.stopped:
                return
            self.run_test('truncated', name, data)

    # ---- 5. 超长帧 ----
    def test_oversized(self):
        frame = self.valid_frame()
        tests = [
            ('plus_100_random', frame + bytes(random.randint(0, 255) for _ in range(100))),
            ('plus_256_zeros', frame + bytes(256)),
            ('plus_256_0xFF', frame + bytes([0xFF] * 256)),
            ('1024_random', frame + bytes(random.randint(0, 255) for _ in range(1024 - len(frame)))),
        ]
        for name, data in tests:
            if self.stopped:
                return
            self.run_test('oversized', name, data)

    # ---- 6. 无效命令码 ----
    def test_commands(self):
        base = list(self.valid_frame())
        for cmd_val in range(256):
            if self.stopped:
                return
            bad = base.copy()
            bad[1] = cmd_val
            bad[-1] = compute_crc(bytes(bad[:-1]))
            name = f'cmd_0x{cmd_val:02X}'
            self.run_test('commands', name, bytes(bad), skip_alive_check=(cmd_val != 0xFF and cmd_val % 16 != 0))

        # 每16个才检查一次存活，最后做一次完整检查
        if not self.stopped:
            alive = self.check_alive()
            self.log(f'[commands/final_check] 设备: {"存活" if alive else "宕机!!"}',
                     'INFO' if alive else 'ERROR')
            if 'commands' in self.summaries:
                self.summaries['commands'].record_alive(alive)

    # ---- 7. 无效地址 ----
    def test_addresses(self):
        base = list(self.valid_frame())
        special = [0x00, 0xFF, 0xFE, (self.device_id + 1) & 0xFF, max(0, self.device_id - 1)]
        randoms = [random.randint(1, 254) for _ in range(20)]
        all_addrs = special + randoms

        for addr in all_addrs:
            if self.stopped:
                return
            bad = base.copy()
            bad[0] = addr
            bad[-1] = compute_crc(bytes(bad[:-1]))
            self.run_test('addresses', f'addr_0x{addr:02X}', bytes(bad))

    # ---- 8. 全0/全1 ----
    def test_zeros_ones(self):
        lengths = [1, 7, 32, 128, 256]
        for length in lengths:
            if self.stopped:
                return
            self.run_test('zeros_ones', f'zeros_{length}', bytes(length))
        for length in lengths:
            if self.stopped:
                return
            self.run_test('zeros_ones', f'ones_{length}', bytes([0xFF] * length))

    # ---- 9. 随机数据 ----
    def test_random(self):
        lengths = [1, 2, 6, 7, 8, 16, 32, 64, 128, 256, 512, 1024]
        for r in range(self.rounds):
            if self.stopped:
                return
            for length in lengths:
                if self.stopped:
                    return
                data = bytes(random.randint(0, 255) for _ in range(length))
                self.run_test('random', f'round{r+1}_len{length}', data)

    # ---- 10. 极速连发 ----
    def test_rapid_fire(self):
        if self.stopped:
            return
        frame = self.valid_frame()
        self.log('[rapid_fire] 发送100帧不等回复...')
        self.flush_buffers()
        blast_data = frame * 100  # 记录实际发送的完整数据
        t0 = time.monotonic()
        send_ok = True
        try:
            for i in range(100):
                if self.stopped:
                    return
                self.port.write(frame)
            self.port.flush()
        except serial.SerialException as e:
            self.log(f'[rapid_fire] 写入错误: {e}', 'ERROR')
            send_ok = False
        elapsed = (time.monotonic() - t0) * 1000

        self._current_step += 1
        if self.progress_callback:
            self.progress_callback(self._current_step, self._total_steps)

        time.sleep(1.0)  # 等设备处理
        alive = self.check_alive()
        self.log(f'[rapid_fire/blast_100] 设备: {"存活" if alive else "宕机!!"}',
                 'INFO' if alive else 'ERROR')

        result = TestResult('rapid_fire', 'blast_100', frame, None, alive, elapsed)
        self.results.append(result)

        if 'rapid_fire' in self.summaries:
            s = self.summaries['rapid_fire']
            s.total = 100
            s.record_alive(alive)

        self._current_step += 1
        if self.progress_callback:
            self.progress_callback(self._current_step, self._total_steps)

    # ---- 11. 时序攻击 ----
    def test_timing(self):
        frame = self.valid_frame()
        delays = [50, 100, 200, 500]
        for d in delays:
            if self.stopped:
                return
            self.run_test('timing', f'byte_delay_{d}ms', frame, slow_delay_ms=d)

        # 半帧等待后发另一半
        half = len(frame) // 2
        split_delays = [500, 1000, 2000, 5000]
        for d in split_delays:
            if self.stopped:
                return
            name = f'split_{d}ms'
            self.log(f'[timing/{name}] TX前半: {format_hex(frame[:half])}')
            self.flush_buffers()
            t0 = time.monotonic()
            try:
                self.port.write(frame[:half])
                self.port.flush()
                time.sleep(d / 1000.0)
                self.port.write(frame[half:])
                self.port.flush()
                resp = self.port.read(1024)
            except serial.SerialException:
                resp = None
            elapsed = (time.monotonic() - t0) * 1000

            if resp:
                self.log(f'[timing/{name}] RX: {format_hex(resp)}')
            else:
                self.log(f'[timing/{name}] RX: <超时>')

            time.sleep(0.05)
            alive = self.check_alive()
            self.log(f'[timing/{name}] 设备: {"存活" if alive else "宕机!!"}',
                     'INFO' if alive else 'ERROR')

            result = TestResult('timing', name, frame, resp if resp else None, alive, elapsed)
            self.results.append(result)

            if 'timing' in self.summaries:
                s = self.summaries['timing']
                s.total += 1
                if resp:
                    s.responded += 1
                s.record_alive(alive)

            self._current_step += 1
            if self.progress_callback:
                self.progress_callback(self._current_step, self._total_steps)

    # ---- 12. 重复字节 ----
    def test_repeated(self):
        repeat_bytes = [0x00, 0xFF, self.device_id, READ_STATUS]
        repeat_lengths = [10, 100, 500]
        for b in repeat_bytes:
            for length in repeat_lengths:
                if self.stopped:
                    return
                data = bytes([b] * length)
                self.run_test('repeated', f'byte_0x{b:02X}_x{length}', data)

    # ---- 13. 帧边界混淆 ----
    def test_boundary(self):
        frame = self.valid_frame()

        # 两帧拼接
        self.run_test('boundary', 'double_frame', frame + frame)
        if self.stopped:
            return

        # 帧 + 垃圾 + 帧
        garbage = bytes(random.randint(0, 255) for _ in range(10))
        self.run_test('boundary', 'frame_garbage_frame', frame + garbage + frame)
        if self.stopped:
            return

        # 帧中间插入垃圾
        mid = len(frame) // 2
        self.run_test('boundary', 'garbage_mid_frame', frame[:mid] + garbage + frame[mid:])
        if self.stopped:
            return

        # 交错两帧字节
        interleaved = bytes(b for pair in zip(frame, frame) for b in pair)
        self.run_test('boundary', 'interleaved', interleaved)
        if self.stopped:
            return

        # 正确帧前加垃圾
        self.run_test('boundary', 'garbage_prefix', garbage + frame)
        if self.stopped:
            return

        # 正确帧后加垃圾
        self.run_test('boundary', 'garbage_suffix', frame + garbage)

    # ---- 14. 恢复验证 ----
    def test_recovery(self):
        for i in range(10):
            if self.stopped:
                return
            self.run_test('recovery', f'recovery_{i+1}', self.valid_frame())
            time.sleep(0.1)


# ============================================================
#  GUI 界面
# ============================================================

class FuzzApp:
    def __init__(self, root: tk.Tk):
        self.root = root
        self.root.title('下位机健壮性测试工具')
        self.root.geometry('900x750')
        self.root.minsize(800, 650)

        self.log_queue: queue.Queue = queue.Queue()
        self.runner: Optional[FuzzRunner] = None
        self.test_thread: Optional[threading.Thread] = None
        self.category_vars: dict[str, tk.BooleanVar] = {}

        self._build_ui()
        self._poll_log_queue()

    def _build_ui(self):
        # ---- 串口设置 ----
        settings_frame = ttk.LabelFrame(self.root, text='串口设置', padding=8)
        settings_frame.pack(fill='x', padx=8, pady=(8, 4))

        row0 = ttk.Frame(settings_frame)
        row0.pack(fill='x')

        ttk.Label(row0, text='端口:').pack(side='left')
        self.port_var = tk.StringVar()
        self.port_combo = ttk.Combobox(row0, textvariable=self.port_var, width=12, state='readonly')
        self.port_combo.pack(side='left', padx=(2, 10))
        ttk.Button(row0, text='刷新', command=self._refresh_ports, width=4).pack(side='left', padx=(0, 10))

        ttk.Label(row0, text='波特率:').pack(side='left')
        self.baud_var = tk.StringVar(value='28800')
        ttk.Entry(row0, textvariable=self.baud_var, width=8).pack(side='left', padx=(2, 10))

        ttk.Label(row0, text='设备ID:').pack(side='left')
        self.devid_var = tk.StringVar(value='11')
        ttk.Entry(row0, textvariable=self.devid_var, width=5).pack(side='left', padx=(2, 10))

        ttk.Label(row0, text='数据长度:').pack(side='left')
        self.datalen_var = tk.StringVar(value='29')
        ttk.Entry(row0, textvariable=self.datalen_var, width=5).pack(side='left', padx=(2, 0))

        row1 = ttk.Frame(settings_frame)
        row1.pack(fill='x', pady=(4, 0))

        ttk.Label(row1, text='帧间延迟(ms):').pack(side='left')
        self.delay_var = tk.StringVar(value='50')
        ttk.Entry(row1, textvariable=self.delay_var, width=6).pack(side='left', padx=(2, 10))

        ttk.Label(row1, text='随机轮次:').pack(side='left')
        self.rounds_var = tk.StringVar(value='10')
        ttk.Entry(row1, textvariable=self.rounds_var, width=5).pack(side='left', padx=(2, 10))

        self._refresh_ports()

        # ---- 测试类别 ----
        cat_frame = ttk.LabelFrame(self.root, text='测试类别', padding=8)
        cat_frame.pack(fill='x', padx=8, pady=4)

        cat_grid = ttk.Frame(cat_frame)
        cat_grid.pack(fill='x')

        for i, (cat_id, cat_name) in enumerate(TEST_CATEGORIES):
            var = tk.BooleanVar(value=True)
            self.category_vars[cat_id] = var
            cb = ttk.Checkbutton(cat_grid, text=cat_name, variable=var)
            cb.grid(row=i // 4, column=i % 4, sticky='w', padx=8, pady=2)

        btn_row = ttk.Frame(cat_frame)
        btn_row.pack(fill='x', pady=(4, 0))
        ttk.Button(btn_row, text='全选', command=self._select_all).pack(side='left', padx=4)
        ttk.Button(btn_row, text='全不选', command=self._select_none).pack(side='left', padx=4)

        # ---- 控制 ----
        ctrl_frame = ttk.Frame(self.root, padding=4)
        ctrl_frame.pack(fill='x', padx=8)

        self.start_btn = ttk.Button(ctrl_frame, text='开始测试', command=self._start_test)
        self.start_btn.pack(side='left', padx=4)

        self.stop_btn = ttk.Button(ctrl_frame, text='停止', command=self._stop_test, state='disabled')
        self.stop_btn.pack(side='left', padx=4)

        self.detect_btn = ttk.Button(ctrl_frame, text='检测设备', command=self._detect_device)
        self.detect_btn.pack(side='left', padx=4)

        self.progress_var = tk.DoubleVar(value=0)
        self.progress_bar = ttk.Progressbar(ctrl_frame, variable=self.progress_var, maximum=100)
        self.progress_bar.pack(side='left', fill='x', expand=True, padx=8)

        self.progress_label = ttk.Label(ctrl_frame, text='0%')
        self.progress_label.pack(side='left')

        # ---- 日志 ----
        log_frame = ttk.LabelFrame(self.root, text='实时日志', padding=4)
        log_frame.pack(fill='both', expand=True, padx=8, pady=4)

        self.log_text = scrolledtext.ScrolledText(log_frame, height=12, font=('Consolas', 9),
                                                   state='disabled', wrap='none')
        self.log_text.pack(fill='both', expand=True)

        self.log_text.tag_configure('INFO', foreground='black')
        self.log_text.tag_configure('ERROR', foreground='red')
        self.log_text.tag_configure('HEADER', foreground='blue', font=('Consolas', 9, 'bold'))
        self.log_text.tag_configure('SUCCESS', foreground='green')

        # ---- 结果汇总 ----
        result_frame = ttk.LabelFrame(self.root, text='测试结果汇总', padding=4)
        result_frame.pack(fill='x', padx=8, pady=(4, 4))

        columns = ('category', 'total', 'responded', 'alive')
        self.result_tree = ttk.Treeview(result_frame, columns=columns, show='headings', height=6)
        self.result_tree.heading('category', text='类别')
        self.result_tree.heading('total', text='总数')
        self.result_tree.heading('responded', text='有响应')
        self.result_tree.heading('alive', text='设备存活')
        self.result_tree.column('category', width=120)
        self.result_tree.column('total', width=80, anchor='center')
        self.result_tree.column('responded', width=80, anchor='center')
        self.result_tree.column('alive', width=100, anchor='center')
        self.result_tree.pack(fill='x')

        self.result_tree.tag_configure('dead', background='#FFD0D0')
        self.result_tree.tag_configure('alive', background='#D0FFD0')

        export_btn = ttk.Button(result_frame, text='导出报告', command=self._export_report)
        export_btn.pack(anchor='e', pady=(4, 0))

    def _refresh_ports(self):
        ports = [p.device for p in serial.tools.list_ports.comports()]
        self.port_combo['values'] = ports
        if ports and not self.port_var.get():
            self.port_var.set(ports[0])

    def _select_all(self):
        for var in self.category_vars.values():
            var.set(True)

    def _select_none(self):
        for var in self.category_vars.values():
            var.set(False)

    def _get_selected_categories(self) -> list[str]:
        return [cat_id for cat_id, var in self.category_vars.items() if var.get()]

    def _validate_inputs(self) -> Optional[tuple]:
        try:
            port = self.port_var.get()
            if not port:
                raise ValueError('请选择串口')
            baud = int(self.baud_var.get())
            device_id = int(self.devid_var.get())
            data_len = int(self.datalen_var.get())
            delay_ms = int(self.delay_var.get())
            rounds = int(self.rounds_var.get())
            if not (0 <= device_id <= 255):
                raise ValueError('设备ID范围: 0-255')
            if not (0 <= data_len <= 255):
                raise ValueError('数据长度范围: 0-255')
            return port, baud, device_id, data_len, delay_ms, rounds
        except ValueError as e:
            messagebox.showerror('参数错误', str(e))
            return None

    def _detect_device(self):
        params = self._validate_inputs()
        if not params:
            return
        port, baud, device_id, data_len, _, _ = params

        def detect():
            try:
                ser = serial.Serial(port, baud, bytesize=serial.EIGHTBITS,
                                    stopbits=serial.STOPBITS_ONE, parity=serial.PARITY_NONE,
                                    timeout=DEFAULT_TIMEOUT)
                frame = build_poll_frame(device_id, data_len)
                self.log_queue.put(('INFO', f'检测设备: TX {format_hex(frame)}'))
                ser.reset_input_buffer()
                ser.write(frame)
                ser.flush()
                resp = ser.read(1024)
                ser.close()
                if is_valid_status_response(resp, device_id, data_len):
                    self.log_queue.put(('SUCCESS', f'设备在线! RX: {format_hex(resp)}'))
                elif resp:
                    self.log_queue.put((
                        'ERROR',
                        f'收到无效响应，设备状态无法确认: RX {format_hex(resp)}'
                    ))
                else:
                    self.log_queue.put(('ERROR', '设备无响应 (超时)'))
            except serial.SerialException as e:
                self.log_queue.put(('ERROR', f'检测失败: {e}'))

        threading.Thread(target=detect, daemon=True).start()

    def _update_progress(self, current: int, total: int):
        # FuzzRunner 在后台线程调用此回调；Tk 控件只能由主线程更新。
        self.log_queue.put(('PROGRESS', (current, total)))

    def _start_test(self):
        params = self._validate_inputs()
        if not params:
            return
        categories = self._get_selected_categories()
        if not categories:
            messagebox.showwarning('提示', '请至少选择一个测试类别')
            return

        port, baud, device_id, data_len, delay_ms, rounds = params

        self.start_btn.config(state='disabled')
        self.stop_btn.config(state='normal')
        self.detect_btn.config(state='disabled')
        self.progress_var.set(0)
        self.progress_label.config(text='0%')

        # 清空日志和结果
        self.log_text.config(state='normal')
        self.log_text.delete('1.0', 'end')
        self.log_text.config(state='disabled')
        for item in self.result_tree.get_children():
            self.result_tree.delete(item)

        self.runner = FuzzRunner(port, baud, device_id, data_len, delay_ms, rounds,
                                 self.log_queue, self._update_progress)

        def run():
            self.runner.run_categories(categories)

        self.test_thread = threading.Thread(target=run, daemon=True)
        self.test_thread.start()

    def _stop_test(self):
        if self.runner:
            self.runner.stop()
            self.log_queue.put(('ERROR', '用户停止测试'))

    def _on_test_done(self, completed: bool):
        self.start_btn.config(state='normal')
        self.stop_btn.config(state='disabled')
        self.detect_btn.config(state='normal')
        if completed:
            self.progress_var.set(100)
            self.progress_label.config(text='100%')
        elif self.runner and self.runner._total_steps > 0:
            pct = min(100, self.runner._current_step / self.runner._total_steps * 100)
            self.progress_var.set(pct)
            self.progress_label.config(text=f'{pct:.0f}% (未完成)')
        else:
            self.progress_var.set(0)
            self.progress_label.config(text='0% (未完成)')

        if self.runner:
            for item in self.result_tree.get_children():
                self.result_tree.delete(item)

            for cat_id, _ in TEST_CATEGORIES:
                if cat_id in self.runner.summaries:
                    s = self.runner.summaries[cat_id]
                    if s.alive_after is None:
                        alive_str = '未检查'
                        tag = ''
                    else:
                        alive_str = 'OK' if s.alive_after else 'DEAD !!'
                        tag = 'alive' if s.alive_after else 'dead'
                    self.result_tree.insert('', 'end',
                                           values=(s.display_name, s.total, s.responded, alive_str),
                                           tags=(tag,) if tag else ())

    def _poll_log_queue(self):
        try:
            while True:
                level, msg = self.log_queue.get_nowait()
                if level == 'DONE':
                    self._on_test_done(bool(msg))
                    continue
                if level == 'PROGRESS':
                    current, total = msg
                    if total > 0:
                        pct = min(100, current / total * 100)
                        self.progress_var.set(pct)
                        self.progress_label.config(text=f'{pct:.0f}%')
                    continue
                self.log_text.config(state='normal')
                self.log_text.insert('end', msg + '\n', level)
                self.log_text.see('end')
                self.log_text.config(state='disabled')
        except queue.Empty:
            pass
        self.root.after(50, self._poll_log_queue)

    def _export_report(self):
        if not self.runner or not self.runner.summaries:
            messagebox.showinfo('提示', '暂无测试结果可导出')
            return

        filepath = filedialog.asksaveasfilename(
            defaultextension='.txt',
            filetypes=[('文本文件', '*.txt'), ('所有文件', '*.*')],
            initialfile=f'fuzz_report_{datetime.now().strftime("%Y%m%d_%H%M%S")}.txt'
        )
        if not filepath:
            return

        with open(filepath, 'w', encoding='utf-8') as f:
            f.write('=' * 60 + '\n')
            f.write('  下位机健壮性测试报告\n')
            f.write(f'  时间: {datetime.now().strftime("%Y-%m-%d %H:%M:%S")}\n')
            f.write(f'  端口: {self.port_var.get()}  波特率: {self.baud_var.get()}\n')
            f.write(f'  设备ID: {self.devid_var.get()}  数据长度: {self.datalen_var.get()}\n')
            f.write('=' * 60 + '\n\n')

            f.write(f'{"类别":<15} {"总数":>6} {"有响应":>8} {"设备存活":>10}\n')
            f.write('-' * 45 + '\n')

            dead_categories = []
            for cat_id, _ in TEST_CATEGORIES:
                if cat_id in self.runner.summaries:
                    s = self.runner.summaries[cat_id]
                    if s.alive_after is None:
                        alive_str = '未检查'
                    else:
                        alive_str = 'OK' if s.alive_after else 'DEAD !!'
                    f.write(f'{s.display_name:<15} {s.total:>6} {s.responded:>8} {alive_str:>10}\n')
                    if s.alive_after is False:
                        dead_categories.append(s.display_name)

            if dead_categories:
                f.write('\n' + '!' * 45 + '\n')
                f.write('导致设备宕机的测试类别:\n')
                for name in dead_categories:
                    f.write(f'  - {name}\n')
                f.write('!' * 45 + '\n')

            f.write('\n\n详细测试记录:\n')
            f.write('-' * 60 + '\n')
            for r in self.runner.results:
                f.write(f'[{r.category}/{r.test_name}]\n')
                f.write(f'  TX: {format_hex(r.sent)}\n')
                if r.response:
                    f.write(f'  RX: {format_hex(r.response)}\n')
                else:
                    f.write(f'  RX: <超时>\n')
                if r.device_alive is None:
                    alive_text = '未检查'
                else:
                    alive_text = '是' if r.device_alive else '否'
                f.write(f'  耗时: {r.duration_ms:.1f}ms  设备存活: {alive_text}\n')

        self.log_queue.put(('SUCCESS', f'报告已导出: {filepath}'))


def main():
    root = tk.Tk()
    app = FuzzApp(root)
    root.mainloop()


if __name__ == '__main__':
    main()
