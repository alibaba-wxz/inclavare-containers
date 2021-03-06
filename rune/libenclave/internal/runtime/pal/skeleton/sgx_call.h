/* SPDX-License-Identifier: GPL-2.0 */
/*
 * Copyright(c) 2016-19 Intel Corporation.
 */

#ifndef SGX_CALL_H
#define SGX_CALL_H

#define ECALL_MAGIC		0
#define MAX_ECALLS		1

#define EEXIT			4

#define INIT_MAGIC		0xdeadfacedeadbeefUL

#ifndef __ASSEMBLER__

#define SGX_ENTER_1_ARG(ecall_num, tcs, a0) \
	({      \
		int __ret; \
		asm volatile( \
			"mov %1, %%r10\n\t" \
			"mov %2, %%r11\n\t" \
			"call sgx_ecall\n\t" \
			: "=a" (__ret) \
			: "r" ((uint64_t)ecall_num), "r" (tcs), \
			  "D" (a0) \
			: "r10", "r11" \
		); \
		__ret; \
	})

#define ENCLU			".byte 0x0f, 0x01, 0xd7"

#else

#define ENCLU			.byte 0x0f, 0x01, 0xd7

#endif

#endif /* SGX_CALL_H */
