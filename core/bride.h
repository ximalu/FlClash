#pragma once

#include <stdlib.h>

extern void (*release_object_func)(void *obj);

extern void (*free_string_func)(char *data);

/* protect 返回非 0 = 该 fd 已被成功排除在 VPN 之外（并绑到物理网络）。 */
extern int (*protect_func)(void *tun_interface, int fd);

extern char* (*resolve_process_func)(void *tun_interface, int protocol, const char *source, const char *target, int uid);

extern void (*result_func)(void *invoke_Interface, const char *data);

extern int protect(void *tun_interface, int fd);

extern char* resolve_process(void *tun_interface, int protocol, const char *source, const char *target, int uid);

extern void release_object(void *obj);

extern void free_string(char *data);

extern void result(void *invoke_Interface,  const char *data);