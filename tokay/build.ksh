#/usr/bin/env ksh

#simple build script for tokay:

df=tokay.df		# default to prod/stable
tag=tokay:latest

export=0
export_only=0

while [[ $1 == -* ]]
do
	case $1 in 
		-e) df=tokay_exp.df; tag="tokay:dev";;
		-x) export=1;;
		-X) export=1; export_only=1;;

		*)	echo "unrecognised option: $1"
			echo "usage: $0 [-e] [-X | -x]"
			exit 1
			;;
	esac

	shift
done



if (( !export_only ))
then
	docker build -t $tag -f $df .
fi

if (( export ))
then
	rm -f tokay.tar
	docker save -o ${tag//:/.}.tar $tag
	ls -al *tar
fi
