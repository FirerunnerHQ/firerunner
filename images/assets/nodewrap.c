/*
 * nodewrap stands in for each node the actions runner bundles (bin/node
 * under externals/nodeNN, the real one moved to node.real) and execs it with
 * NODE_EXTRA_CA_CERTS naming externals/firerunner-ca.pem, the cache proxy's
 * CA, when that file is there (a tier that caches; see
 * firerunner-cache-setup.sh). Node reads no system store, and externals is
 * mounted into job containers at /__e, so the path relative to this binary
 * holds on the host and in a container alike.
 *
 * C, statically linked: it must run in any job container (glibc or musl),
 * and it must not be a shell script, since POSIX sh drops environment
 * variables whose names are not identifiers, and the runner passes action
 * inputs as INPUT_<NAME> with the input's dashes intact.
 */
#include <libgen.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	char self[PATH_MAX], real[PATH_MAX], ca[PATH_MAX];
	ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);

	(void)argc;
	if (n < 0) {
		perror("nodewrap: /proc/self/exe");
		return 127;
	}
	self[n] = '\0';
	const char *dir = dirname(self); /* .../externals/nodeNN/bin */
	if (snprintf(real, sizeof real, "%s/node.real", dir) >= (int)sizeof real ||
	    snprintf(ca, sizeof ca, "%s/../../firerunner-ca.pem", dir) >= (int)sizeof ca) {
		fputs("nodewrap: path too long\n", stderr);
		return 127;
	}
	if (getenv("NODE_EXTRA_CA_CERTS") == NULL && access(ca, R_OK) == 0)
		setenv("NODE_EXTRA_CA_CERTS", ca, 1);
	argv[0] = real;
	execv(real, argv);
	fprintf(stderr, "nodewrap: exec %s: ", real);
	perror(NULL);
	return 127;
}
