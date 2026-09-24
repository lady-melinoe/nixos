// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode kernel data plane -- skeleton.
 *
 * This is currently just enough to load, register nothing, and unload
 * cleanly, so the Nix build/packaging plumbing can be exercised before the
 * genl family (see melnode_genl.h and dpproto/API.md) is implemented.
 */

#include <linux/module.h>
#include <linux/init.h>

#include "melnode_genl.h"

static int __init melnode_init(void)
{
	pr_info("melnode: loaded (skeleton, family \"%s\" v%d not yet registered)\n",
		MELNODE_GENL_NAME, MELNODE_GENL_VERSION);
	return 0;
}

static void __exit melnode_exit(void)
{
	pr_info("melnode: unloaded\n");
}

module_init(melnode_init);
module_exit(melnode_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("melnode kernel data plane (skeleton)");
