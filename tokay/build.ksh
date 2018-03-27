#/usr/bin/env ksh

#simple build script for tokay:

if [[ $1 != "-X" ]]				# -X export only; -x export after build
then
	docker build -t tokay -f tokay.df .
fi

if [[ $1 == "-x" || $1 == "-X" ]]
then
	rm -f tokay.tar
	docker save -o tokay.tar tokay:latest
fi
