/* Standalone Linux READ_STATUS probe. No Rust, OPC, retries or driver changes.
 * gcc -std=c11 -O2 -Wall -Wextra -Werror serial_parity_probe.c -o probe
 * probe PORT BAUD SECONDS ROUNDS delay|drain|flush raw|marked|nul|ignore|discard ID:LEN[,ID:LEN...]
 * ROUNDS=0 runs for SECONDS. Every physical query is reported independently.
 */
#define _GNU_SOURCE
#include <asm/termbits.h>
#include <asm/ioctls.h>
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/serial.h>
#include <poll.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

#define CAP 2048
#define MAX_DEVICES 128
static volatile sig_atomic_t stopped;
static volatile sig_atomic_t alarm_fired;
static const char *stage = "initialization";
struct options {
    unsigned pre_ms, address_margin_ms, payload_margin_ms, gap_ms, timeout_ms, clear_input;
};
static struct options options = {25, 25, 25, 0, 300, 1};

struct counts { unsigned queries, valid, timeout, zero, bad, io_errors, marked, cancelled; };
struct device { uint8_t id, len; struct counts totals; };
struct bytes {
    uint8_t raw[CAP], decoded[CAP];
    size_t raw_len, len;
    unsigned state, errors, zero_errors, invalid, overflow;
    unsigned chunks, error_positions[64], error_position_count;
    int64_t first_read_us, last_read_us;
};

static int64_t now_us(void) {
    struct timespec t;
    if (clock_gettime(CLOCK_MONOTONIC, &t)) abort();
    return (int64_t)t.tv_sec * 1000000 + t.tv_nsec / 1000;
}
static void signal_handler(int n) {
    if (n == SIGALRM) alarm_fired = 1;
    else stopped = 1;
}
static int bounded_ioctl(int fd, unsigned long request, uintptr_t arg) {
    struct itimerval timer = {.it_value = {.tv_sec = 2}};
    alarm_fired = 0;
    if (setitimer(ITIMER_REAL, &timer, NULL)) return -1;
    int rc = ioctl(fd, request, arg);
    int saved = errno;
    memset(&timer, 0, sizeof timer);
    if (setitimer(ITIMER_REAL, &timer, NULL)) return -1;
    errno = alarm_fired ? ETIMEDOUT : saved;
    return alarm_fired ? -1 : rc;
}
static int sleep_us(int64_t us) {
    struct timespec t = {.tv_sec = us / 1000000, .tv_nsec = (us % 1000000) * 1000};
    while (nanosleep(&t, &t)) {
        if (errno != EINTR || stopped) return -1;
    }
    return 0;
}
static int wait_ready(int fd, short events, int64_t deadline) {
    while (!stopped) {
        int64_t left = deadline - now_us();
        if (left <= 0) { errno = ETIMEDOUT; return 0; }
        struct pollfd p = {.fd = fd, .events = events};
        int rc = poll(&p, 1, (int)((left + 999) / 1000));
        if (rc < 0 && errno == EINTR) continue;
        if (rc <= 0) { if (!rc) errno = ETIMEDOUT; return rc; }
        if (p.revents & (POLLERR | POLLHUP | POLLNVAL)) { errno = EIO; return -1; }
        if (p.revents & events) return 1;
    }
    errno = EINTR;
    return -1;
}
static int write_bounded(int fd, const uint8_t *p, size_t len) {
    int64_t deadline = now_us() + 2000000;
    while (len) {
        if (wait_ready(fd, POLLOUT, deadline) <= 0) return -1;
        ssize_t n = write(fd, p, len);
        if (n < 0 && (errno == EINTR || errno == EAGAIN)) continue;
        if (n <= 0) { if (!n) errno = EIO; return -1; }
        p += n;
        len -= (size_t)n;
    }
    return 0;
}
static void push_byte(struct bytes *b, uint8_t c) {
    if (b->len < CAP) b->decoded[b->len++] = c;
    else b->overflow++;
}
static void feed(struct bytes *b, const uint8_t *p, size_t len, bool marked) {
    for (size_t i = 0; i < len; i++) {
        uint8_t c = p[i];
        if (b->raw_len < CAP) b->raw[b->raw_len++] = c;
        else b->overflow++;
        if (!marked) { push_byte(b, c); continue; }
        if (!b->state) {
            if (c == 0xff) b->state = 1;
            else push_byte(b, c);
        } else if (b->state == 1) {
            if (c == 0) b->state = 2;
            else {
                if (c != 0xff) { b->invalid++; push_byte(b, 0xff); }
                push_byte(b, c);
                b->state = 0;
            }
        } else {
            b->errors++;
            if (!c) b->zero_errors++;
            if (b->error_position_count < 64) b->error_positions[b->error_position_count++] = (unsigned)b->len;
            push_byte(b, c);
            b->state = 0;
        }
    }
}
static size_t expected_len(const struct bytes *b, unsigned configured) {
    if (b->len < 3) return 0;
    size_t declared = (size_t)b->decoded[2] + 5;
    size_t minimum = configured + ((unsigned)b->decoded[2] + 1 == configured ? 4 : 5);
    return declared > minimum ? declared : minimum;
}
static int read_response(int fd, unsigned configured, bool marked, struct bytes *b) {
    int64_t started = now_us(), deadline = started + options.timeout_ms * 1000LL;
    while (!stopped) {
        size_t need = expected_len(b, configured);
        if (need && b->len >= need && !b->state) return 1;
        int rc = wait_ready(fd, POLLIN, deadline);
        if (rc <= 0) return rc;
        uint8_t chunk[512];
        ssize_t n = read(fd, chunk, sizeof chunk);
        if (n < 0 && (errno == EINTR || errno == EAGAIN)) continue;
        if (n < 0) return -1;
        if (!n) continue;
        if (!b->chunks) b->first_read_us = now_us() - started;
        b->last_read_us = now_us() - started;
        b->chunks++;
        feed(b, chunk, (size_t)n, marked);
        if (b->overflow) { errno = EOVERFLOW; return -1; }
    }
    errno = EINTR;
    return -1;
}
static uint8_t sum(const uint8_t *p, size_t n) {
    unsigned value = 0;
    for (size_t i = 0; i < n; i++) value += p[i];
    return (uint8_t)value;
}
static const char *classify(const struct bytes *b) {
    if (!b->len) return "timeout";
    bool zero = true;
    for (size_t i = 0; i < b->len; i++) if (b->decoded[i]) zero = false;
    if (zero) return "all_zero";
    if (b->state || b->invalid) return "marker_error";
    if (b->len < 5 || (size_t)b->decoded[2] + 5 != b->len) return "length_error";
    if (sum(b->decoded, b->len - 1) != b->decoded[b->len - 1]) return "checksum_error";
    return "valid";
}
static void hex(const uint8_t *p, size_t n) {
    putchar('"');
    for (size_t i = 0; i < n && i < 64; i++) printf("%s%02X", i ? " " : "", p[i]);
    putchar('"');
}
static int apply_parity(int fd, struct termios2 *t, bool mark, bool drain) {
    t->c_cflag |= PARENB | CMSPAR;
    if (mark) t->c_cflag |= PARODD;
    else t->c_cflag &= ~PARODD;
    return bounded_ioctl(fd, drain ? TCSETSW2 : TCSETS2, (uintptr_t)t);
}
static int send_query(int fd, struct termios2 *t, bool drain, bool flush, const uint8_t *frame,
                      unsigned baud, int64_t *address_us, int64_t *payload_us) {
    int64_t start = now_us();
    stage = "set_mark";
    if (apply_parity(fd, t, true, drain)) return -1;
    stage = "pre_address_delay";
    if (sleep_us(options.pre_ms * 1000LL)) return -1;
    stage = "write_address";
    if (write_bounded(fd, frame, 1)) return -1;
    stage = "address_settle";
    if (flush) {
        if (bounded_ioctl(fd, TCSBRK, 1)) return -1;
    } else if (!drain && sleep_us(options.address_margin_ms * 1000LL + (12000000LL + baud - 1) / baud)) return -1;
    stage = "set_space";
    if (apply_parity(fd, t, false, drain)) return -1;
    *address_us = now_us() - start;
    start = now_us();
    stage = "write_payload";
    if (write_bounded(fd, frame + 1, 6)) return -1;
    stage = "payload_settle";
    if (drain || flush) {
        /* TCSBRK with nonzero argument is Linux tcdrain, not buffer discard. */
        if (bounded_ioctl(fd, TCSBRK, 1)) return -1;
    } else if (sleep_us(options.payload_margin_ms * 1000LL + (72000000LL + baud - 1) / baud)) return -1;
    *payload_us = now_us() - start;
    return 0;
}
static void print_icount(int fd, const char *when) {
    struct serial_icounter_struct c;
    memset(&c, 0, sizeof c);
    int rc = ioctl(fd, TIOCGICOUNT, &c);
    printf("{\"type\":\"icount\",\"when\":\"%s\",\"supported\":%s,\"errno\":%d,"
           "\"rx\":%d,\"tx\":%d,\"parity\":%d,\"frame\":%d,\"overrun\":%d,\"brk\":%d,\"buf_overrun\":%d}\n",
           when, rc ? "false" : "true", rc ? errno : 0,
           c.rx, c.tx, c.parity, c.frame, c.overrun, c.brk, c.buf_overrun);
}
static void self_test(void) {
    uint8_t wire[] = {1, 0xff, 0xff, 0xff, 0, 42, 0xff, 0, 0};
    uint8_t decoded[] = {1, 0xff, 42, 0};
    for (size_t split = 0; split <= sizeof wire; split++) {
        struct bytes b = {0};
        feed(&b, wire, split, true);
        feed(&b, wire + split, sizeof wire - split, true);
        assert(b.len == sizeof decoded && !memcmp(b.decoded, decoded, sizeof decoded));
        assert(b.errors == 2 && b.zero_errors == 1 && !b.state && !b.invalid);
        assert(b.error_position_count == 2 && b.error_positions[0] == 2 && b.error_positions[1] == 3);
    }
    struct bytes b = {0};
    uint8_t frame[] = {0, 8, 1, 0x12, 0x34, 0x4f};
    feed(&b, frame, sizeof frame, false);
    assert(!strcmp(classify(&b), "valid"));
    assert(expected_len(&b, 1) == 6 && expected_len(&b, 2) == 6);
    b.decoded[5]++;
    assert(!strcmp(classify(&b), "checksum_error"));
    b.len--;
    assert(!strcmp(classify(&b), "length_error"));
    memset(&b, 0, sizeof b);
    assert(!strcmp(classify(&b), "timeout"));
    b.len = 34;
    assert(!strcmp(classify(&b), "all_zero"));
    puts("SELF_TEST_OK checksum frame_length zero_detection split_parmrk literal_ff");
}
static unsigned parse_unsigned(const char *s, unsigned max) {
    char *end;
    errno = 0;
    unsigned long v = strtoul(s, &end, 10);
    if (errno || !*s || *end || v > max) { fputs("invalid integer\n", stderr); exit(2); }
    return (unsigned)v;
}
static unsigned env_unsigned(const char *name, unsigned fallback, unsigned max) {
    const char *value = getenv(name);
    return value ? parse_unsigned(value, max) : fallback;
}
int main(int argc, char **argv) {
    if (argc == 2 && !strcmp(argv[1], "--self-test")) { self_test(); return 0; }
    if (argc != 8) {
        fprintf(stderr, "usage: %s PORT BAUD SECONDS ROUNDS delay|drain|flush raw|marked|nul|ignore|discard ID:LEN,...\n", argv[0]);
        return 2;
    }
    const char *port = argv[1];
    if (strspn(port, "/_-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != strlen(port)) return 2;
    unsigned baud = parse_unsigned(argv[2], 4000000);
    unsigned seconds = parse_unsigned(argv[3], 86400), rounds = parse_unsigned(argv[4], 1000000);
    if (!baud || !seconds) return 2;
    bool drain = !strcmp(argv[5], "drain"), flush = !strcmp(argv[5], "flush");
    bool marked = !strcmp(argv[6], "marked");
    tcflag_t input_flags = 0;
    if (marked) input_flags = INPCK | PARMRK;
    else if (!strcmp(argv[6], "nul")) input_flags = INPCK;
    else if (!strcmp(argv[6], "ignore")) input_flags = IGNPAR;
    else if (!strcmp(argv[6], "discard")) input_flags = INPCK | IGNPAR;
    else if (strcmp(argv[6], "raw")) return 2;
    if (!drain && !flush && strcmp(argv[5], "delay")) return 2;
    options.pre_ms = env_unsigned("PROBE_PRE_MS", 25, 1000);
    options.address_margin_ms = env_unsigned("PROBE_ADDRESS_MARGIN_MS", 25, 1000);
    options.payload_margin_ms = env_unsigned("PROBE_PAYLOAD_MARGIN_MS", 25, 1000);
    options.gap_ms = env_unsigned("PROBE_GAP_MS", 0, 5000);
    options.timeout_ms = env_unsigned("PROBE_TIMEOUT_MS", 300, 5000);
    options.clear_input = env_unsigned("PROBE_CLEAR_INPUT", 1, 1);
    if (!options.timeout_ms) return 2;
    struct device devices[MAX_DEVICES] = {0};
    unsigned count = 0;
    char *spec = strdup(argv[7]), *save = NULL;
    if (!spec) return 2;
    for (char *p = strtok_r(spec, ",", &save); p; p = strtok_r(NULL, ",", &save)) {
        char *colon = strchr(p, ':');
        if (!colon || count == MAX_DEVICES) return 2;
        *colon++ = 0;
        devices[count].id = (uint8_t)parse_unsigned(p, 255);
        devices[count++].len = (uint8_t)parse_unsigned(colon, 255);
    }
    free(spec);
    if (!count) return 2;
    struct sigaction action = {.sa_handler = signal_handler};
    sigemptyset(&action.sa_mask);
    if (sigaction(SIGALRM, &action, NULL) || sigaction(SIGINT, &action, NULL) ||
        sigaction(SIGTERM, &action, NULL)) return 2;
    setvbuf(stdout, NULL, _IOLBF, 0);
    int fd = open(port, O_RDWR | O_NOCTTY | O_NONBLOCK | O_CLOEXEC);
    if (fd < 0) { perror("open"); return 1; }
    if (ioctl(fd, TIOCEXCL)) { perror("exclusive"); close(fd); return 1; }
    struct termios2 original;
    if (ioctl(fd, TCGETS2, &original)) { perror("TCGETS2"); close(fd); return 1; }
    struct termios2 settings = original;
    settings.c_iflag = input_flags;
    settings.c_oflag = 0;
    settings.c_lflag = 0;
    settings.c_cflag = BOTHER | CS8 | CLOCAL | CREAD | PARENB | CMSPAR;
    settings.c_ispeed = settings.c_ospeed = baud;
    settings.c_cc[VMIN] = 0;
    settings.c_cc[VTIME] = 0;
    if (bounded_ioctl(fd, TCSETS2, (uintptr_t)&settings)) {
        perror("configure");
        if (bounded_ioctl(fd, TCSETS2, (uintptr_t)&original)) perror("restore_termios");
        close(fd); return 1;
    }
    struct termios2 actual;
    if (ioctl(fd, TCGETS2, &actual)) {
        perror("readback");
        if (bounded_ioctl(fd, TCSETS2, (uintptr_t)&original)) perror("restore_termios");
        close(fd); return 1;
    }
    printf("{\"type\":\"start\",\"port\":\"%s\",\"baud\":%u,\"mode\":\"%s\",\"rx_mode\":\"%s\","
           "\"seconds\":%u,\"rounds\":%u,\"devices\":%u,\"iflag\":%u,\"cflag\":%u,\"ispeed\":%u,\"ospeed\":%u}\n",
           port, baud, argv[5], argv[6], seconds, rounds, count,
           actual.c_iflag, actual.c_cflag, actual.c_ispeed, actual.c_ospeed);
    printf("{\"type\":\"options\",\"pre_ms\":%u,\"address_margin_ms\":%u,\"payload_margin_ms\":%u,"
           "\"gap_ms\":%u,\"timeout_ms\":%u,\"clear_input\":%u}\n", options.pre_ms,
           options.address_margin_ms, options.payload_margin_ms, options.gap_ms, options.timeout_ms, options.clear_input);
    int failed = 0;
    if (actual.c_iflag != settings.c_iflag || actual.c_ispeed != baud || actual.c_ospeed != baud ||
        (actual.c_cflag & (CSIZE | PARENB | CMSPAR | PARODD | CSTOPB | CRTSCTS)) !=
        (settings.c_cflag & (CSIZE | PARENB | CMSPAR | PARODD | CSTOPB | CRTSCTS))) {
        fputs("termios readback mismatch\n", stderr);
        failed = 1;
    }
    print_icount(fd, "start");
    int64_t started = now_us(), end = started + (int64_t)seconds * 1000000;
    unsigned round = 0, seq = 0;
    while (!failed && !stopped && now_us() < end && (!rounds || round < rounds)) {
        round++;
        for (unsigned d = 0; d < count && !stopped && now_us() < end; d++) {
            struct device *dev = &devices[d];
            uint8_t frame[7] = {dev->id, 8, 2, 0x30, 0x10, dev->len, 0};
            frame[6] = sum(frame, 6);
            struct bytes b = {0};
            stage = "clear_input";
            int pending_before = -1;
            (void)ioctl(fd, FIONREAD, &pending_before);
            int rc = options.clear_input ? ioctl(fd, TCFLSH, TCIFLUSH) : 0, saved_errno = 0;
            int64_t query_start = now_us(), address_us = 0, payload_us = 0;
            if (!rc) rc = send_query(fd, &settings, drain, flush, frame, baud, &address_us, &payload_us);
            if (!rc) { stage = "read"; rc = read_response(fd, dev->len, marked, &b); }
            if (rc < 0) saved_errno = errno;
            const char *result = stopped && rc < 0 && saved_errno == EINTR ? "cancelled" : rc < 0 ? "io_error" : classify(&b);
            dev->totals.queries++;
            if (!strcmp(result, "valid")) dev->totals.valid++;
            else if (!strcmp(result, "timeout")) dev->totals.timeout++;
            else if (!strcmp(result, "all_zero")) dev->totals.zero++;
            else if (!strcmp(result, "cancelled")) dev->totals.cancelled++;
            else if (rc < 0) { dev->totals.io_errors++; failed = 1; }
            else dev->totals.bad++;
            if (b.errors) dev->totals.marked++;
            printf("{\"type\":\"query\",\"seq\":%u,\"round\":%u,\"id\":%u,\"requested_len\":%u,"
                   "\"result\":\"%s\",\"stage\":\"%s\",\"errno\":%d,\"elapsed_us\":%lld,\"since_start_us\":%lld,"
                   "\"address_us\":%lld,\"payload_us\":%lld,\"read_complete\":%s,\"raw_len\":%zu,\"len\":%zu,"
                   "\"reply_addr\":%d,\"reply_cmd\":%d,\"declared_len\":%d,\"error_marks\":%u,\"zero_error_marks\":%u,"
                   "\"marker_state\":%u,\"raw_hex\":",
                   ++seq, round, dev->id, dev->len, result, stage, saved_errno,
                   (long long)(now_us() - query_start), (long long)(now_us() - started),
                   (long long)address_us, (long long)payload_us, rc == 1 ? "true" : "false", b.raw_len, b.len,
                   b.len ? b.decoded[0] : -1, b.len > 1 ? b.decoded[1] : -1, b.len > 2 ? b.decoded[2] : -1,
                   b.errors, b.zero_errors, b.state);
            hex(b.raw, b.raw_len);
            printf(",\"decoded_hex\":"); hex(b.decoded, b.len);
            printf(",\"truncated\":%s,\"chunks\":%u,\"first_read_us\":%lld,\"last_read_us\":%lld,"
                   "\"pending_before\":%d,\"error_positions\":[", b.raw_len > 64 || b.len > 64 ? "true" : "false",
                   b.chunks, (long long)b.first_read_us, (long long)b.last_read_us, pending_before);
            for (unsigned i = 0; i < b.error_position_count; i++) printf("%s%u", i ? "," : "", b.error_positions[i]);
            puts("]}");
            if (failed) break;
            if (!stopped && options.gap_ms && sleep_us(options.gap_ms * 1000LL)) {
                if (!stopped) { perror("inter_query_gap"); failed = 1; }
                break;
            }
        }
    }
    print_icount(fd, "end");
    for (unsigned d = 0; d < count; d++) {
        struct counts *c = &devices[d].totals;
        printf("{\"type\":\"summary\",\"id\":%u,\"requested_len\":%u,\"queries\":%u,\"valid\":%u,"
               "\"timeouts\":%u,\"all_zero\":%u,\"bad\":%u,\"io_errors\":%u,\"marked_queries\":%u,\"cancelled\":%u}\n",
               devices[d].id, devices[d].len, c->queries, c->valid, c->timeout, c->zero, c->bad, c->io_errors, c->marked, c->cancelled);
    }
    printf("{\"type\":\"end\",\"queries\":%u,\"duration_us\":%lld,\"failed\":%s,\"interrupted\":%s}\n",
           seq, (long long)(now_us() - started), failed ? "true" : "false", stopped ? "true" : "false");
    if (bounded_ioctl(fd, TCSETS2, (uintptr_t)&original)) { perror("restore_termios"); failed = 1; }
    if (ioctl(fd, TIOCNXCL)) { perror("release_exclusive"); failed = 1; }
    close(fd);
    return failed ? 1 : 0;
}
