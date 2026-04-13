#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <liburing.h>
#include <linux/if_tun.h>
#include <linux/netlink.h>
#include <linux/rtnetlink.h>
#include <net/if.h>
#include <netinet/in.h>
#include <poll.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/socket.h>
#include <sys/uio.h>
#include <unistd.h>

#define DEFAULT_PORT 6969
#define DEFAULT_BATCH 32
#define MAX_BATCH 1024
#define PKT_BUF_SZ 2048

struct runtime {
    int tun_fd;
    int sock_fd;
    int batch;
    volatile sig_atomic_t stop;
};

static struct runtime *g_rt = NULL;

/* ---------------- utils ---------------- */

static void die(const char *msg) {
    perror(msg);
    exit(1);
}

static void diex(const char *msg) {
    fprintf(stderr, "%s\n", msg);
    exit(1);
}

static void on_signal(int signo) {
    (void)signo;
    if (g_rt)
        g_rt->stop = 1;
}

static int open_netns_fd(const char *nsname) {
    char path[256];
    snprintf(path, sizeof(path), "/var/run/netns/%s", nsname);
    return open(path, O_RDONLY | O_CLOEXEC);
}

static void enter_namespace(const char *nsname) {
    int fd = open_netns_fd(nsname);
    if (fd < 0)
        die("open_netns_fd");

    if (setns(fd, CLONE_NEWNET) < 0) {
        close(fd);
        die("setns");
    }

    close(fd);
}

static int with_namespace(const char *nsname, int (*fn)(void *), void *arg) {
    int orig = open("/proc/self/ns/net", O_RDONLY | O_CLOEXEC);
    if (orig < 0)
        return -1;

    int target = open_netns_fd(nsname);
    if (target < 0) {
        close(orig);
        return -1;
    }

    if (setns(target, CLONE_NEWNET) < 0) {
        close(target);
        close(orig);
        return -1;
    }

    int rc = fn(arg);

    if (setns(orig, CLONE_NEWNET) < 0) {
        close(target);
        close(orig);
        return -1;
    }

    close(target);
    close(orig);
    return rc;
}

static void parse_endpoint(const char *in, struct sockaddr_in *out, int default_port) {
    char buf[128];
    snprintf(buf, sizeof(buf), "%s", in);

    char *colon = strchr(buf, ':');
    int port = default_port;

    if (colon) {
        *colon = 0;
        port = atoi(colon + 1);
    }

    memset(out, 0, sizeof(*out));
    out->sin_family = AF_INET;
    out->sin_port = htons(port);

    if (inet_pton(AF_INET, buf, &out->sin_addr) != 1)
        diex("invalid IP");
}

/* ---------------- netlink helpers ---------------- */

static int nl_talk_ack(int sock, struct nlmsghdr *nlh) {
    char buf[8192];
    struct sockaddr_nl sa = { .nl_family = AF_NETLINK };

    struct iovec iov = {
        .iov_base = nlh,
        .iov_len = nlh->nlmsg_len
    };

    struct msghdr msg = {
        .msg_name = &sa,
        .msg_namelen = sizeof(sa),
        .msg_iov = &iov,
        .msg_iovlen = 1
    };

    if (sendmsg(sock, &msg, 0) < 0)
        return -1;

    for (;;) {
        struct iovec riov = { .iov_base = buf, .iov_len = sizeof(buf) };
        struct msghdr rmsg = {
            .msg_name = &sa,
            .msg_namelen = sizeof(sa),
            .msg_iov = &riov,
            .msg_iovlen = 1
        };

        ssize_t n = recvmsg(sock, &rmsg, 0);
        if (n < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }

        for (struct nlmsghdr *h = (struct nlmsghdr *)buf;
             NLMSG_OK(h, n);
             h = NLMSG_NEXT(h, n)) {

            if (h->nlmsg_type == NLMSG_ERROR) {
                struct nlmsgerr *err = (struct nlmsgerr *)NLMSG_DATA(h);
                if (err->error == 0)
                    return 0;
                errno = -err->error;
                return -1;
            }
        }
    }
}

static int ifindex_by_name(const char *ifname) {
    return if_nametoindex(ifname);
}

/* ---------------- TUN ---------------- */

static int create_tun(const char *name) {
    struct ifreq ifr = {0};

    int fd = open("/dev/net/tun", O_RDWR | O_CLOEXEC);
    if (fd < 0)
        die("open tun");

    snprintf(ifr.ifr_name, IFNAMSIZ, "%s", name);
    ifr.ifr_flags = IFF_TUN | IFF_NO_PI;

    if (ioctl(fd, TUNSETIFF, &ifr) < 0) {
        close(fd);
        die("TUNSETIFF");
    }

    int flags = fcntl(fd, F_GETFL, 0);
    if (flags >= 0)
        fcntl(fd, F_SETFL, flags | O_NONBLOCK);

    return fd;
}

/* ---------------- link up ---------------- */

static int set_link_up_impl(void *arg) {
    const char *ifname = arg;

    int idx = ifindex_by_name(ifname);
    if (idx <= 0)
        return -1;

    int sock = socket(AF_NETLINK, SOCK_RAW | SOCK_CLOEXEC, NETLINK_ROUTE);
    if (sock < 0)
        return -1;

    struct {
        struct nlmsghdr nlh;
        struct ifinfomsg ifi;
    } req = {0};

    req.nlh.nlmsg_len = NLMSG_LENGTH(sizeof(struct ifinfomsg));
    req.nlh.nlmsg_type = RTM_NEWLINK;
    req.nlh.nlmsg_flags = NLM_F_REQUEST | NLM_F_ACK;

    req.ifi.ifi_index = idx;
    req.ifi.ifi_flags = IFF_UP;
    req.ifi.ifi_change = IFF_UP;

    int rc = nl_talk_ack(sock, &req.nlh);

    close(sock);
    return rc;
}

static void set_link_up_in_ns(const char *ns, const char *ifname) {
    if (!ns) {
        if (set_link_up_impl((void *)ifname) < 0)
            die("set_link_up");
    } else {
        if (with_namespace(ns, set_link_up_impl, (void *)ifname) < 0)
            die("set_link_up ns");
    }
}

/* ---------------- move to namespace ---------------- */

static int move_to_namespace(const char *ifname, const char *nsname) {
    int ns_fd = open_netns_fd(nsname);
    if (ns_fd < 0)
        return -1;

    int idx = ifindex_by_name(ifname);
    if (idx <= 0) {
        close(ns_fd);
        return -1;
    }

    int sock = socket(AF_NETLINK, SOCK_RAW | SOCK_CLOEXEC, NETLINK_ROUTE);
    if (sock < 0) {
        close(ns_fd);
        return -1;
    }

    struct {
        struct nlmsghdr nlh;
        struct ifinfomsg ifi;
        char buf[256];
    } req = {0};

    req.nlh.nlmsg_len = NLMSG_LENGTH(sizeof(struct ifinfomsg));
    req.nlh.nlmsg_type = RTM_NEWLINK;
    req.nlh.nlmsg_flags = NLM_F_REQUEST | NLM_F_ACK;

    req.ifi.ifi_index = idx;

    struct rtattr *rta = (void *)((char *)&req + NLMSG_ALIGN(req.nlh.nlmsg_len));
    rta->rta_type = IFLA_NET_NS_FD;
    rta->rta_len = RTA_LENGTH(sizeof(int));
    memcpy(RTA_DATA(rta), &ns_fd, sizeof(int));

    req.nlh.nlmsg_len = NLMSG_ALIGN(req.nlh.nlmsg_len) + RTA_LENGTH(sizeof(int));

    int rc = nl_talk_ack(sock, &req.nlh);

    close(sock);
    close(ns_fd);
    return rc;
}

/* ---------------- io_uring helpers ---------------- */

static int submit_ring(struct io_uring *ring) {
    int rc = io_uring_submit(ring);
    if (rc < 0) {
        errno = -rc;
        return -1;
    }
    return 0;
}

static int ring_init(struct io_uring *ring, unsigned entries) {
    int rc = io_uring_queue_init(entries, ring, 0);
    if (rc < 0) {
        errno = -rc;
        return -1;
    }
    return 0;
}

static bool is_tun_write_fatal(int err) {
    return err != EINTR && err != EAGAIN && err != EBADF && err != ENODEV;
}

static int reap_write_completions(struct io_uring *ring, bool wait_for_one, bool *fatal) {
    struct io_uring_cqe *cqes[128];
    unsigned count = io_uring_peek_batch_cqe(ring, cqes, 128);

    if (count == 0 && wait_for_one) {
        struct io_uring_cqe *cqe = NULL;
        int rc = io_uring_wait_cqe(ring, &cqe);
        if (rc < 0) {
            errno = -rc;
            perror("io_uring_wait_cqe");
            *fatal = true;
            return -1;
        }
        cqes[0] = cqe;
        count = 1;
    }

    for (unsigned i = 0; i < count; i++) {
        struct io_uring_cqe *cqe = cqes[i];

        if (cqe->res < 0) {
            int e = -cqe->res;
            if (e == EBADF || e == ENODEV) {
                *fatal = true;
            } else if (is_tun_write_fatal(e)) {
                errno = e;
                perror("write tun");
                *fatal = true;
            }
        }
    }

    if (count > 0)
        io_uring_cq_advance(ring, count);

    return (int)count;
}

/* ---------------- threads ---------------- */

static void *tun_to_udp_thread(void *opaque) {
    struct runtime *rt = opaque;
    struct pollfd pfd = {
        .fd = rt->tun_fd,
        .events = POLLIN
    };

    unsigned char (*bufs)[PKT_BUF_SZ] = NULL;
    if (posix_memalign((void **)&bufs, 64, (size_t)rt->batch * sizeof(*bufs)) != 0)
        diex("posix_memalign tun bufs");

    struct mmsghdr *msgs = calloc((size_t)rt->batch, sizeof(*msgs));
    struct iovec *iov = calloc((size_t)rt->batch, sizeof(*iov));
    if (!msgs || !iov)
        die("calloc tun_to_udp");

    for (int i = 0; i < rt->batch; i++) {
        iov[i].iov_base = bufs[i];
        msgs[i].msg_hdr.msg_iov = &iov[i];
        msgs[i].msg_hdr.msg_iovlen = 1;
    }

    while (!rt->stop) {
        int pr = poll(&pfd, 1, -1);
        if (pr < 0) {
            if (errno == EINTR)
                continue;
            perror("poll tun");
            break;
        }

        if (!(pfd.revents & (POLLIN | POLLERR | POLLHUP)))
            continue;

        int count = 0;

        for (int i = 0; i < rt->batch; i++) {
            ssize_t n = read(rt->tun_fd, bufs[i], PKT_BUF_SZ);

            if (n > 0) {
                iov[i].iov_len = (size_t)n;
                count++;
                continue;
            }

            if (n < 0 && errno == EINTR)
                continue;

            if (n < 0 && errno == EAGAIN)
                break;

            if (n < 0) {
                perror("read tun");
                rt->stop = 1;
            }
            break;
        }

        if (rt->stop)
            break;

        if (count == 0)
            continue;

        int off = 0;
        while (off < count && !rt->stop) {
            int sent = sendmmsg(rt->sock_fd, &msgs[off], (unsigned)(count - off), 0);
            if (sent < 0) {
                if (errno == EINTR)
                    continue;
                if (errno == EAGAIN || errno == ECONNREFUSED)
                    break;
                perror("sendmmsg");
                rt->stop = 1;
                break;
            }
            off += sent;
        }
    }

    free(iov);
    free(msgs);
    free(bufs);
    return NULL;
}

static void *udp_to_tun_thread(void *opaque) {
    struct runtime *rt = opaque;
    struct io_uring ring;

    int ring_entries = rt->batch * 2;
    if (ring_entries < 64)
        ring_entries = 64;

    if (ring_init(&ring, (unsigned)ring_entries) < 0)
        die("io_uring_queue_init");

    unsigned char (*bufs)[PKT_BUF_SZ] = NULL;
    if (posix_memalign((void **)&bufs, 64, (size_t)rt->batch * sizeof(*bufs)) != 0)
        diex("posix_memalign udp bufs");

    struct mmsghdr *msgs = calloc((size_t)rt->batch, sizeof(*msgs));
    struct iovec *iov = calloc((size_t)rt->batch, sizeof(*iov));
    if (!msgs || !iov)
        die("calloc udp_to_tun");

    for (int i = 0; i < rt->batch; i++) {
        iov[i].iov_base = bufs[i];
        iov[i].iov_len = PKT_BUF_SZ;
        msgs[i].msg_hdr.msg_iov = &iov[i];
        msgs[i].msg_hdr.msg_iovlen = 1;
    }

    while (!rt->stop) {
        int n = recvmmsg(rt->sock_fd, msgs, (unsigned)rt->batch, MSG_WAITFORONE, NULL);
        if (n < 0) {
            if (errno == EINTR || errno == EAGAIN || errno == ECONNREFUSED)
                continue;
            perror("recvmmsg");
            break;
        }

        bool fatal = false;
        int queued = 0;

        for (int i = 0; i < n; i++) {
            struct io_uring_sqe *sqe = io_uring_get_sqe(&ring);

            while (!sqe) {
                if (submit_ring(&ring) < 0) {
                    perror("io_uring_submit");
                    fatal = true;
                    break;
                }

                queued = 0;

                if (reap_write_completions(&ring, true, &fatal) < 0 || fatal)
                    break;

                sqe = io_uring_get_sqe(&ring);
            }

            if (fatal)
                break;

            io_uring_prep_write(sqe, rt->tun_fd, bufs[i], msgs[i].msg_len, (off_t)-1);
            io_uring_sqe_set_data64(sqe, (uint64_t)i);
            queued++;
        }

        if (fatal)
            break;

        if (queued > 0) {
            if (submit_ring(&ring) < 0) {
                perror("io_uring_submit");
                break;
            }
        }

        if (reap_write_completions(&ring, false, &fatal) < 0 || fatal)
            break;
    }

    bool fatal = false;
    for (;;) {
        unsigned pending = io_uring_sq_ready(&ring);
        if (pending == 0)
            break;

        if (submit_ring(&ring) < 0) {
            perror("io_uring_submit");
            break;
        }

        if (reap_write_completions(&ring, true, &fatal) < 0 || fatal)
            break;
    }

    while (!fatal) {
        int reaped = reap_write_completions(&ring, false, &fatal);
        if (reaped <= 0)
            break;
    }

    io_uring_queue_exit(&ring);
    free(iov);
    free(msgs);
    free(bufs);
    return NULL;
}

/* ---------------- main ---------------- */

int main(int argc, char *argv[]) {
    char *source = NULL, *dest = NULL, *name = NULL;
    char *sns = NULL, *tns = NULL;
    int port = DEFAULT_PORT, batch = DEFAULT_BATCH;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--source"))
            source = argv[++i];
        else if (!strcmp(argv[i], "--dest"))
            dest = argv[++i];
        else if (!strcmp(argv[i], "--name"))
            name = argv[++i];
        else if (!strcmp(argv[i], "--sns"))
            sns = argv[++i];
        else if (!strcmp(argv[i], "--tns"))
            tns = argv[++i];
        else if (!strcmp(argv[i], "--batchsize"))
            batch = atoi(argv[++i]);
    }

    if (!source || !dest || !name)
        diex("missing args");

    struct runtime rt = {0};
    rt.batch = (batch > 0 && batch <= MAX_BATCH) ? batch : DEFAULT_BATCH;
    g_rt = &rt;

    signal(SIGINT, on_signal);

    if (sns)
        enter_namespace(sns);

    rt.sock_fd = socket(AF_INET, SOCK_DGRAM, 0);
    if (rt.sock_fd < 0)
        die("socket");

    rt.tun_fd = create_tun(name);

    if (tns) {
        if (move_to_namespace(name, tns) < 0)
            die("move_to_namespace");
    }

    set_link_up_in_ns(tns, name);

    struct sockaddr_in l = {0}, p = {0};
    parse_endpoint(source, &l, port);
    parse_endpoint(dest, &p, port);

    if (bind(rt.sock_fd, (void *)&l, sizeof(l)) < 0)
        die("bind");

    if (connect(rt.sock_fd, (void *)&p, sizeof(p)) < 0)
        die("connect");

    pthread_t t1, t2;
    if (pthread_create(&t1, NULL, tun_to_udp_thread, &rt) != 0)
        diex("pthread_create tun_to_udp_thread");

    if (pthread_create(&t2, NULL, udp_to_tun_thread, &rt) != 0)
        diex("pthread_create udp_to_tun_thread");

    while (!rt.stop)
        pause();

    close(rt.sock_fd);
    close(rt.tun_fd);

    pthread_join(t1, NULL);
    pthread_join(t2, NULL);

    return 0;
}
