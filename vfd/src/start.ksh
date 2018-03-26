#!/usr/bin/env ksh
# :vi sw=4 noet ts=4 :

#	Mneminic:	start
#	Abstract:	The container start script for VFd. 
#
#				This script _requires_ Korn shell because bash does NOT grok
#				assigning/updating variables in a loop.
#
#	Date:		13 February 2018
#	Author:		E. Scott Daniels
# ----------------------------------------------------------------------

function dt {
	echo $(date +"%Y/%m/%d.%H:%M:%S")
}

function log_msg {
	echo "$(dt -p) $1"
}

# Ensure that a directory ($1) exists. If the file $1/MOUNTPT exists, we 
# assumed that a filesystem would be mounted on top of this, hiding MOUNTPT,
# thus if we see it we fail.  If -w is given, then we test to ensure the 
# directory is writable before confirming that it exists.
#
function ensured {
	typeset test_write=0
	while [[ $1 == -* ]]
	do
		case $1 in 
			-w) test_write=1;;
		esac
		shift
	done

	if [[ ! -d $1 ]]
	then
		log_msg "abort: not a directory: $1" 
		abort=1
		return
	fi

	if [[ -e $1/MOUNTPT ]]		# if we epected something mounted here, and it's not, fail
	then
		log_msg "abort: $1 was not mounted from an outside filesystem and must be"
		abort=1
		return
	fi

	if (( ! test_write ))
	then
		return
	fi

	if ! touch $1/foo >/dev/null 2>&1
	then
		log_msg "abort: cannot write to directory: $1" 
		abort=1
	else
		rm $1/foo
	fi
}

# Ensure that the requested file exists
#
function ensuref {
	if [[ ! -f $1 ]]
	then
		log_msg "abort: cannot find requrired file: $1" 
		abort=1
	fi
}

# Verify that it seems huge pages are available and the the mount point
# is there. 
#
function check_hp {
	if [[ ! -d /mnt/huge ]]
	then
		abort=1
		log_msg "abort: cannot verify that huge pages mountpoint exists (/mnt/huge)"
		return
	fi

	msg=$( awk '
		/HugePages_Total:/	{ total = $NF; next; }
		/HugePages_Free:/	{ free = $NF; next; }
		/HugePages_Rsvd:/	{ next; }
		/HugePages_Surp:/	{ next; }
		/Hugepagesize:/		{ 
			size = $(NF-1); 
			if( $NF == "kB" )  {
				size *= 1024;
			} else {
				if( $NF == "gB" ) {
					size *= 1024 * 1024 * 1024;
				}
			}

			next; 
		}
		END {
			if( (free * size) < (256 * (1024 * 1024)) ) {	# dpdk requires 64M/device
				printf( "abort: huge pages shortfall: found %d kB, need at least %d kB\n", (free*size)/1024, 256 * 1024 * 1024 );
			}
		}

	' /proc/meminfo  )

	if [[ -n $msg ]]
	then
		abort=1
		log_msg "$msg"
		return
	fi
}

#
# Quick sanity check that it seems PCI devices can be found that also have VFs allocated
#
function check_devs {
	typeset c

	if [[ ! -d /sys/devices ]]
	then
		abort=1
		log_msg "abort: unable to find /sys/devices directory"
		return
	fi

	c=$( ls /sys/devices 2>/dev/null |grep -ci pci )
	if (( ! c ))
	then
		abort=1
		log_msg "abort: unable to find any PCI directories in /sys/devices"
		return
	fi

	found_vfs=0
	find /sys/devices -name "sriov_numvfs" | while read f
	do
		found_vfs=$(( found_vfs + $(cat $f) ))
	done

	if (( ! found_vfs ))
	then
		find . -name "max_vfs" | while read f
		do
			found_vfs=$(( found_vfs + $(cat $f) ))
		done
	fi

	if (( ! found_vfs ))
	then
		abort=1
		log_msg "abort: did not find any PCI device with configured VFs"
		return	
	fi
}

# ----------------------------------------------------------------------------------------------
abort=0


while [[ $1 == -* ]]
do
	case $1 in 
		*)	log_msg "unrecognised command line flag: $1" 
			exit 1
	esac

	shift
done

# sanity checks set abort to true. We'll report all problems then exit if needed
# ensure that all directories we expect are both there, and have something mounted
# on top of them (MOUNTPT file deposited during build is not visible).
ensured /etc/vfd
ensured /var/lib
ensured -w /var/lib/vfd/config
ensured -w /var/lib/vfd/config_live
ensured -w /var/lib/vfd/bin
ensured -w /var/log/vfd
ensuref /etc/vfd/vfd.cfg

check_hp
check_devs

if (( abort ))					# some verification failed, bug out now
then
	exit 1
fi

# make iplex visible to external users/containers by placing a copy in /var/lib/vfd/bin
# which external users can use to interact with VFd in the container.
#
if ! mkdir -p /var/lib/vfd/bin
then
	log_msg "abort: unable to create /var/lib/vfd/bin"
	exit 1
fi

# assume Tokay container will keep iplex and other bin utils up to date, but if
# tokay isn't being used, then ensure that they get there.
#
if [[ ! -e /var/lib/vfd/bin/iplex ]]
then
	if ! cp bin/iplex bin/vfd_req /var/lib/vfd/bin
	then
		log_msg "abort: unable to copy iplex to /var/lib/vfd/bin"
		exit 1
	fi

	echo "added iplex and tools to /var/lib/vfd/bin"
else
	echo "iplex already exists in /var/lib/vfd/bin, not replaced"
fi

# at this point three and green, so start. Use -f to prevent container exit until vfd stops
/playpen/bin/vfd -f >/var/log/vfd/vfd.std 2>&1


