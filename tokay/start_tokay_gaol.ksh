
# simple example of how to start the tokay container
# no guarentee that this will work for you!

function ensured {
	while [[ -n $1 ]]
	do
		if [[ ! -d $1 ]]
		then
			echo "abort: canot find $1"
			exit 1
		fi

		shift
	done
}

ensured /var/log/tokay /var/lib/vfd/pipes /var/lib/vfd/bin /var/lib/vfd/config

vfd_pipe_fs="--mount source=/var/lib/vfd/pipes,target=/var/lib/vfd/pipes,type=bind"
vfd_config_fs="--mount source=/var/lib/vfd/config,target=/var/lib/vfd/config,type=bind"
vfd_bin_fs="--mount source=/var/lib/vfd/bin,target=/var/lib/vfd/bin,type=bind"
log_fs="--mount source=/var/log/tokay,target=/var/log/tokay,type=bind"

# specify -it mksh on command line to start interfactive container
set -x
docker run ${1--d --rm} $vfd_pipe_fs $vfd_config_fs $vfd_bin_fs $log_fs  tokay:latest $2
