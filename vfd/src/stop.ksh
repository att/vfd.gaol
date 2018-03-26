#!/usr/bin/env ksh
# :vi sw=4 noet ts=4 :

#	Mneminic:	stop
#	Abstract:	can be executed to stop VFd in the container:
#					docker exec --it <container-id> stop
#
#	Date:		14 March 2018
#	Author:		E. Scott Daniels
# ----------------------------------------------------------------------

function dt {
	echo $(date +"%Y/%m/%d.%H:%M:%S")
}

ps -elf|grep "bin/[v]fd" | awk '{print $4}'|read pid
kill -15 $pid
echo "VFd signaled to stop"
echo "$(dt -p) stopped VFd" >>/var/log/vfd/stop.log
