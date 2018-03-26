#!/usr/bin/env ksh
# :vi ts=4 sw=4 noet :

#	Abstract:	Switch to the VFd directory after checkout and apply all
#				of the patches that are associated with the branch of DPDK
#				that is being built. We assume that patches are named
# 				dpdk<yymm>-*, where yymm is the year/month of the DPDK
#				version being used (e.g. 1802 for v18.02).  The version 
#				given on the command line may be either YYMM or vYY.MM
#
#				If there are no patches for the given version the script will
#				exit with a good return code, and will exit with an error if
#				there are patches which cannot be applied. 
#
#	Author:		E. Scott Daniels
#	Date:		20 March 2017
#----------------------------------------------------------------------------

function usage
{
	echo "usage: $0 [-n] {yymm | vyy.mm[_rcxx]}"
	echo "   where yymm is the year/month of the dpdk release"
	echo "   -n is no execute mode"
}

# ---------------------------------------------------------------------------
VFD_PATH=$PWD

forreal=""
verify=0
action="appling: "
past_action="applied"
patch_dir=.

while [[ $1 == -* ]]
do
	case $1 in 
		-d)		VFD_PATH=$2; shift;;
		-n) 	forreal="echo would execute: ";;

		-s)		
				action="summarise: "
				check="--check"
				past_action="summarised"
				summary="--stat --summary"
				;;

		-V)		verify=1;;
		-v)		verbose="-v";;

		*)	usage
			exit 1
			;;
	esac

	shift
done


if [[ -z $1 ]]
then
	usage
	exit 1
fi

if [[ ! -d $VFD_PATH ]]
then
	echo "patch directory smells: $VFD_PATH"
	exit 1
fi

if [[ ! -d $RTE_SDK ]]
then
	echo "cannot find dpdk directory: ${RTE_SDK:-RTE_SDK is not set}"
	exit 1
fi


if [[ $1 == "v"*"."* ]]				# something like v18.02 given
then
	version=${1//[v.]/}
	version=${version%%_*}			# chop _rc2 or something similar
else
	version=$1
fi

patches=$( ls $VFD_PATH/dpdk$version-* 2>/dev/null )

if [[ -z $patches ]]
then
	echo "there are no patches for dpdk$1"
	exit 0
fi

if [[ ! -d ${RTE_SDK} ]]
then
	echo "abort: cannot find rte sdk directory: ${RTE_SDK:-undefined}"
	echo "ensure RTE_SDK is defined and exported"
	exit 1
fi

for patch in $patches
do
	echo ""

	if (( verify ))				# prompt user to let them pick patches to apply from the list
	then
		printf "verify: apply $patch ?"
		read ans
		if [[ $ans != y* ]]
		then
			continue
		fi
	fi

	(
		cd $RTE_SDK

		if [[ -n $check ]]
		then
			git apply  $check --ignore-whitespace $VFD_PATH/$patch	>/dev/null 2>&1
			if (( $? != 0 ))
			then
				echo " ### Patch already applied, or is not valid for this version: $patch"
			fi
		fi

		if [[ -z $forreal ]]
		then
			echo "$action $patch"
		fi
		$forreal git apply $verbose $summary --ignore-whitespace $patch	# if summary is on, apply is off
	)
	if (( $? != 0 ))
	then
		echo "abort: error applying patch $patch"
		exit 1
	fi

	echo "patch successfully $past_action"
done
