#!/usr/bin/env ksh
# vi: ts=4 sw=4 noet :

#
#	Mnemonic:	build.ksh
#	Abstract:	Build a VFd docker image or a DPDK docker image.  There are three
#				possibile ways to generate an image:
#					-d -- build just a dpdk build environment image with VFd
#						patches applied.
#					-f -- (no fetch) use local source rather than VFd from github to build
#						the VFd image.
#					neither option -- fetch VFd and DPDK from github and build
#						the VFd image.
#
#					When fetching source from VFd the -b option supplies the 
#					vfd and dpdk branches to checkout in the form  vfd:dpdk
#					(e.g. nic_agnostic:v18.02).
#
#				The dpdk image, left dangling as an intermediate, is huge (>1G)
#				while the final VFd image is modest (~250M). Keeping the intermediate
#				image round is beneficial only when fiddling with build issues;
#				once the VFd image is created it's usually wise to ditch the large
#				intermediates.
#
#				If the -x option is given (except with -d), then the VFd image is exported 
#				into a tar file that can be sent to, then loaded into, another docker 
#				environment.
#
#	Date:		14 Mar 2018
#	Author:		E. Scott Daniels
# ------------------------------------------------------------------------


function export_target {
	verbose 1 "exporting the image to a tar file: $target_name.tar"
	if ! rm -f $target_name.tar
	then
		log_msg "abort: could not remove export file before creating new one: $target_name.tar"
		exit 1
	fi

	docker save -o $target_name.tar $target_name
	return $?
}

function fail_msg
{
	echo "something seems to have failed"
}

# single line tail
function stail {
	typeset whitespace=" \t"						# cannot use \t in the subs expression below, but this works

	tail -f $1 | while read buffer
	do
		if [[ -n ${buffer//[$whitespace]/} ]]		# don't spit out blank lines
		then
			printf "%s\033[0K\r" "$buffer"
		fi
	done 
}

function ouch_msg
{
	printf "\nouch, that ctl-C hurts\n"
	trap - EXIT
	exit 2
}

# copy the file from the command line and then ensure that their
# mode is set per $1. Parms:
#		mode file [file...] directory
#
#	if dest file is *.ksh, the .ksh is removed.
function old_cpmod
{
	mode=$1
	shift
	cp "$@"

	typeset flist="$@"
	typeset list=( "$@" )
	typeset n=${#list[@]}
	typeset targetd=${list[$n-1]}

	for (( i = 0; i < n - 1; i++ ))
	do
		verbose 1 "copy-mod ${list[$i]} to playpen"
		f=${list[$i]}
		chmod $mode $targetd/${f##*/}
	done
}

# accept mode dir f1, f2, ... fn and copy files to 
# the diretory.  If filename is of the form sf:df then df is
# used as the destination.
function cpmod {
	typeset mode=$1
	shift

	typeset target_dir=$1
	shift

	for fname in "$@"
	do
		if [[ $fname == *:* ]]
		then
			srcf=${fname%%:*}
			dstf=${fname##*:}
		else
			srcf=$fname
			dstf=${fname##*/}
		fi

		echo "copy $srcf --> $target_dir/$dstf"
		if ! cp $srcf $target_dir/$dstf
		then
			return 1
		else
			chmod $mode $target_dir/$dstf
		fi
	done

	return 0
}

function verbose {
	if (( verbose_level >= $1 ))
	then
		shift
		printf "%s\n" "$@"
	fi
}

# ------ specific build functions ------------------------------------------------------------

# set up the thermo nuclear docker file to build both vfd and dpdk rather than
# installing from various parts of the local fileystem.  This leaves a HUGE intermediate
# container lying round; it should be deleted so as not to clog the works.
#
function pull_n_build {
	if [[ $branch == *":"* ]]				# if user supplied vfd-branch:dpdk-branch
	then
		dpdk_branch="${branch##*:}"
		branch="${branch%%:*}"
	fi

	touch /tmp/PID$$.build
	if (( verbose_level )) 
	then
		stail /tmp/PID$$.build &
		kid=$!
	fi
	$forreal docker build --build-arg dpdk_branch=$dpdk_branch --build-arg vfd_branch=$branch -t $target_name -f vfd.df . >>/tmp/PID$$.build 2>&1
	rc=$?

	if [[ -n $forreal ]]
	then
		cat /tmp/PID$$.build
		return 0
	fi

	if [[ -n $kid ]]
	then
		kill -9 $kid
	fi

	if (( keep_log || rc != 0 ))
	then
		printf "\nlog saved: build_dpdk.log\n"
		mv /tmp/PID$$.build build_dpdk.log
	fi

	rm -f /tmp/PID$$.*
	echo ""
}

#
# build a stand-alone dpdk image.  This is useful if needing to experiment with the 
# VFd build in the dpdk container environment.  It will be HUGE!
#
function build_dpdk {
	if [[ $branch == *":"* ]]				# if user supplied vfd-branch:dpdk-branch
	then
		dpdk_branch="${branch##*:}"
		branch="${branch%%:*}"
	fi

	touch /tmp/PID$$.build
	if (( verbose_level )) 
	then
		stail /tmp/PID$$.build &
		kid=$!
	fi
	$forreal docker build --build-arg dpdk_branch=$dpdk_branch --build-arg vfd_branch=$branch -t dpdk_$dpdk_branch -f dpdk.df . >>/tmp/PID$$.build 2>&1 
	rc=$?

	if [[ -n $forreal ]]
	then
		cat /tmp/PID$$.build
		return 0
	fi

	touch /tmp/PID$$.build
	if (( verbose_level )) 
	then
		stail /tmp/PID$$.build &
		kid=$!
	fi

	if [[ -n $kid ]]
	then
		kill -9 $kid
	fi

	if (( keep_log || rc != 0 ))
	then
		printf "\nlog saved: build_dpdk.log\n"
		mv /tmp/PID$$.build build_dpdk.log
	fi

	rm -f /tmp/PID$$.*
	echo ""
	exit $rc
}

# ------------------------------------------------------------------

vfd_src=..
target_name=vfd_in_gaol			# container name
verbose_level=1					# default to chatty; use -q (quiet) to disable
keep_log=0
export=0

fetch=1							# default to building the current checked in version (master)
branch="master:master"			# vfd-branch:dpdk-branch
dpdk_ver=""
dpdk_only=0

while [[ $1 == -* ]]
do
	case $1 in
		-b)	branch="$2"; shift;;
		-f)	fetch=0;;					# build the container using the vfd stuff from the local environment
		-d)	dpdk_only=1;;				# no fetch, build just a dpdk container
		-k)	keep_log=1;;
		-n) verbose_level=0; forreal="echo would run: ";;
		-s)	vfd_src=$2; shift;;
		-t)	target_name=$2; shift;;
		-q)	verbose_level=0;;
		-x)	export=1;;
		-X)	export=1; export_only=1;;
		*)	
			echo "unrecognised option: $1"
			echo "usage: $0 [-k] [-s vfd-src-dir] [-t target-name] [-q] [-x|-X]"
			echo "-f turns off fetch mode; build will use the vfd from the local environment"
			echo "-d builds a container with only dpdk after applying VFd patches"
			echo "-k == keep log after successful build"
			echo "-x == export after build, -X export only, assumes image exists in docker"
			exit 1
			;;
	esac

	shift
done


if (( export_only ))
then
	set -e
	trap "fail_msg" EXIT
	trap "ouch_msg" SIGINT
	export_target
	trap - EXIT
	exit $?
fi

if (( dpdk_only ))
then
	build_dpdk
	exit $?
fi

if (( fetch ))
then
	if pull_n_build
	then
		if (( export ))
		then
			export_target
		fi
	fi
	exit $?
fi


if [[ ! -d $vfd_src/src/vfd ]]		# -s not found, and we're not at home, see if we can find vfd
then
	if [[ -d ../vfd/src/vfd ]]		# if gaol is maintained in parallel to vfd
	then
		vfd_src="../vfd"
		verbose 1 "detected vfd source in: $vfd_src"
	else
		verbose 0 "abort: source ($vfd_src) doesn't smell good"
		exit 1
	fi
fi

trap "fail_msg" EXIT
trap "ouch_msg" SIGINT
set -e

if [[ -e ./playpen ]]
then	
	printf "playpen directory exists; clear, use, abort [C|u|a] "
	read ans

	case ${ans:-c} in
		a|A)
			trap - EXIT
			exit 0
			;;

		u|U)
			if [[ ! -d ./playpen ]]
			then
				verbose 0 "abort: playpen exists, but isn't a directory"
				trap - EXIT
				exit 1
			fi
			;;

		c|C)
			rm -fr ./playpen
			;;
	esac

fi


# since we cannot reference files 'above' this directory
# we need to create a playpen and put what we need there
# so that it will be 1) small and 2) available during the 
# build.  The playpen we create will have sub directories
# to help separate things out.
#

verbose 1 "making playpen directories"
mkdir -p ./playpen/etc ./playpen/bin 

# copy from build and ensure that mode is set correctly
cpmod 755 ./playpen/bin $vfd_src/src/vfd/build/app/vfd $vfd_src/src/system/vfd_req $vfd_src/src/system/iplex 
cpmod 755 ./playpen/bin src/start.ksh:start src/stop.ksh:stop

rc=0
verbose 1 "build the container: $target_name"
touch /tmp/PID$$.build
if (( verbose_level )) 
then
	stail /tmp/PID$$.build &
	kid=$!
fi
set +e
$forreal docker build -t $target_name -f vfd_local.df . >>/tmp/PID$$.build 2>&1
rc=$?

if [[ -n $kid ]]
then
	kill -9 $kid
	echo ""
fi

if (( rc )) 
then
	verbose 0 "docker build failed, log saved in docker_build.log"
	mv /tmp/PID$$.build ./docker_build.log
else
	if (( keep_log ))
	then
		echo "docker build log saved in docker_build.log"
		mv /tmp/PID$$.build ./docker_build.log
	fi
fi
set -e

if (( export && rc == 0 ))
then
	export_target
fi

verbose 1 "cleaning up"
rm -fr ./playpen /tmp/PID$$.*
trap - EXIT SIGINT
verbose 1  "all done"
exit $rc
