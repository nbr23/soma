pipeline {
    agent any
    options {
        disableConcurrentBuilds()
    }
    stages {
        stage('Checkout'){
            steps {
                checkout scm
            }
        }
        stage('Build') {
            steps {
                script {
                    env.REAL_PWD = getDockerPWD();
                    sh 'docker run --rm -w /app -v $REAL_PWD:/app golang:alpine go build'
                }
            }
        }
        stage('Test') {
            steps {
                script {
                    env.REAL_PWD = getDockerPWD();
                    sh 'docker run --rm -w /app -v $REAL_PWD:/app golang:alpine go test ./...'
                }
            }
        }
        stage('Sync github repos') {
            when { branch 'master' }
            steps {
                syncRemoteBranch('git@github.com:nbr23/soma.git', 'master')
            }
        }
    }
}
